package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

const PROCESS_TERMINATE = 0x0001


const (
	NvdaExeName     = "nvda.exe"
	NarratorExeName = "Narrator.exe"
	PcTalkerExeName = "PCTALKER.exe"
	ConfigFileName  = "config.json"
	DefaultTimeout  = 10
)

// Config は設定ファイルの構造を表します
type Config struct {
	Readers    []string `json:"readers"`
	TimeoutSec int      `json:"timeout_sec,omitempty"`
}

// Reader は1つのスクリーンリーダーの情報を保持します
type Reader struct {
	ID          string
	DisplayName string
	ExeName     string
	Path        string
}

var defaultConfig = Config{
	Readers:    []string{"nvda", "narrator", "pctalker"},
	TimeoutSec: DefaultTimeout,
}

func configPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ConfigFileName
	}
	return filepath.Join(filepath.Dir(exe), ConfigFileName)
}

func loadConfig() Config {
	path := configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		// 設定ファイルが存在しない、または読み込めない場合は自動生成せず
		// デフォルト設定をメモリ上で使用します（UAC回避と管理者権限のないフォルダでの起動を保証するため）
		return defaultConfig
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil || len(cfg.Readers) == 0 {
		return defaultConfig
	}
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = DefaultTimeout
	}
	return cfg
}

// findNvdaPath は NVDA のインストールパスを検索して返します
func findNvdaPath() string {
	paths := []string{
		os.Getenv("ProgramFiles(x86)") + "\\NVDA\\nvda.exe",
		os.Getenv("ProgramFiles") + "\\NVDA\\nvda.exe",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// findNarratorPath は Narrator.exe の存在を確認して返します
func findNarratorPath() string {
	candidates := []string{
		os.Getenv("SystemRoot") + "\\System32\\Narrator.exe",
		"C:\\Windows\\System32\\Narrator.exe",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// findPcTalkerPath は PC-Talker のインストールパスを複数のバージョン・場所から検索して返します
func findPcTalkerPath() string {
	pf := os.Getenv("ProgramFiles")
	pfx := os.Getenv("ProgramFiles(x86)")
	paths := []string{
		pf + "\\KSD\\PCTalker\\PCTALKER.exe",
		pfx + "\\KSD\\PCTalker\\PCTALKER.exe",
		pf + "\\KSD\\PCTalker Neo\\PCTALKER.exe",
		pfx + "\\KSD\\PCTalker Neo\\PCTALKER.exe",
		pf + "\\KSD\\PCTalker10\\PCTALKER.exe",
		pfx + "\\KSD\\PCTalker10\\PCTALKER.exe",
		pf + "\\KSD\\PCTalker 10\\PCTALKER.exe",
		pfx + "\\KSD\\PCTalker 10\\PCTALKER.exe",
		"C:\\KSD\\PCTalker\\PCTALKER.exe",
		"C:\\KSD\\PCTalker Neo\\PCTALKER.exe",
		"C:\\KSD\\PCTalker10\\PCTALKER.exe",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func buildReaderList(cfg Config) []Reader {
	allReaders := map[string]Reader{
		"nvda": {
			ID: "nvda", DisplayName: "NVDA",
			ExeName: NvdaExeName, Path: findNvdaPath(),
		},
		"narrator": {
			ID: "narrator", DisplayName: "Windowsナレーター",
			ExeName: NarratorExeName, Path: findNarratorPath(),
		},
		"pctalker": {
			ID: "pctalker", DisplayName: "PC-Talker Neo",
			ExeName: PcTalkerExeName, Path: findPcTalkerPath(),
		},
	}

	var list []Reader
	for _, id := range cfg.Readers {
		r, ok := allReaders[strings.ToLower(id)]
		if !ok || r.Path == "" {
			continue
		}
		list = append(list, r)
	}
	return list
}

// findCurrentReaderID は現在起動中のスクリーンリーダーのIDを返します。
// 起動中のものがなければ最初のリーダーのIDを返します。
func findCurrentReaderID(readers []Reader) string {
	if len(readers) == 0 {
		return ""
	}
	for _, r := range readers {
		if isRunning(r.ExeName) {
			return r.ID
		}
	}
	return readers[0].ID
}

// encodePSCommand はPowerShellの -EncodedCommand 用に
// スクリプト文字列を UTF-16LE base64 エンコードして返します。
// 一時ファイル不要で日本語を確実に渡せます。
func encodePSCommand(script string) string {
	u16 := utf16.Encode([]rune(script))
	b := make([]byte, len(u16)*2)
	for i, r := range u16 {
		b[i*2] = byte(r)
		b[i*2+1] = byte(r >> 8)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// showError はエラーメッセージをメッセージボックスで表示します
func showError(msg string) {
	escaped := strings.ReplaceAll(msg, "'", "''")
	script := fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms
[System.Windows.Forms.MessageBox]::Show('%s', 'エラー', 'OK', 'Error') | Out-Null`, escaped)
	newGuiCmd("powershell",
		"-NoProfile",
		"-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encodePSCommand(script),
	).Run()
}

// showSelectionDialog は WinForms ダイアログでスクリーンリーダーを選択させます。
// PowerShellスクリプトを -EncodedCommand (UTF-16LE base64) で渡すため
// 一時ファイルなしに日本語文字列を正しく扱えます。
// 戻り値: 選択されたスクリーンリーダーID。キャンセル時は ""
func showSelectionDialog(readers []Reader, defaultID string) string {
	if len(readers) == 0 {
		return ""
	}

	// defaultID が readers 内に存在するインデックスを特定（なければ 0）
	defaultIdx := 0
	for i, r := range readers {
		if r.ID == defaultID {
			defaultIdx = i
			break
		}
	}

	// 各サイズと余白の定義 (ロービジョン向けに大きく確保)
	formWidth := 460
	rowHeight := 45
	startY := 60
	btnHeight := 36
	btnWidth := 120

	helpY := startY + len(readers)*rowHeight + 5
	btnY := helpY + 30
	formHeight := btnY + btnHeight + 60
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$form = New-Object System.Windows.Forms.Form
$form.Text = "スクリーンリーダー切り替え"
$form.Size = New-Object System.Drawing.Size(%d, %d)
$form.StartPosition = "CenterScreen"
$form.FormBorderStyle = "FixedDialog"
$form.MaximizeBox = $false
$form.MinimizeBox = $false
$form.TopMost = $true
$form.BackColor = [System.Drawing.Color]::FromArgb(26, 26, 26)

$titleFont = New-Object System.Drawing.Font("Yu Gothic UI", 12, [System.Drawing.FontStyle]::Bold)
$rbFont = New-Object System.Drawing.Font("Yu Gothic UI", 12, [System.Drawing.FontStyle]::Bold)
$btnFont = New-Object System.Drawing.Font("Yu Gothic UI", 11, [System.Drawing.FontStyle]::Bold)
$helpFont = New-Object System.Drawing.Font("Yu Gothic UI", 9.5, [System.Drawing.FontStyle]::Regular)

$label = New-Object System.Windows.Forms.Label
$label.Text = "起動するスクリーンリーダーを選択してください:"
$label.AutoSize = $false
$label.Size = New-Object System.Drawing.Size(420, 30)
$label.Location = New-Object System.Drawing.Point(20, 20)
$label.Font = $titleFont
$label.ForeColor = [System.Drawing.Color]::FromArgb(240, 240, 240)
$form.Controls.Add($label)

$rbs = @()
`, formWidth, formHeight))

	for i, r := range readers {
		label := fmt.Sprintf("%d. %s", i+1, r.DisplayName)
		if isRunning(r.ExeName) {
			label += "  （現在起動中）"
		}
		// PowerShell文字列内のダブルクォートをエスケープ
		label = strings.ReplaceAll(label, `"`, "`\"")
		checked := "false"
		if i == defaultIdx {
			checked = "true"
		}
		yPos := startY + i*rowHeight
		sb.WriteString(fmt.Sprintf(`$rb%d = New-Object System.Windows.Forms.RadioButton
$rb%d.Text = "%s"
$rb%d.AutoSize = $false
$rb%d.Size = New-Object System.Drawing.Size(410, 38)
$rb%d.Location = New-Object System.Drawing.Point(25, %d)
$rb%d.Font = $rbFont
$rb%d.Checked = $%s
$rb%d.ForeColor = [System.Drawing.Color]::FromArgb(220, 220, 220)
$rb%d.FlatStyle = [System.Windows.Forms.FlatStyle]::Flat
$rb%d.FlatAppearance.BorderSize = 2
$rb%d.FlatAppearance.CheckedBackColor = [System.Drawing.Color]::FromArgb(45, 45, 45)
$rb%d.Add_GotFocus({
	$this.BackColor = [System.Drawing.Color]::FromArgb(55, 55, 55)
	$this.ForeColor = [System.Drawing.Color]::Gold
})
$rb%d.Add_LostFocus({
	$this.BackColor = [System.Drawing.Color]::Transparent
	$this.ForeColor = [System.Drawing.Color]::FromArgb(220, 220, 220)
})
$form.Controls.Add($rb%d)
$rbs += $rb%d
`, i, i, label, i, i, i, yPos, i, i, checked, i, i, i, i, i, i, i, i))
	}

	sb.WriteString(fmt.Sprintf(`
$helpLabel = New-Object System.Windows.Forms.Label
$helpLabel.Text = "※数字キー（1～%d）で選択、Enterキーで起動できます。"
$helpLabel.AutoSize = $false
$helpLabel.Size = New-Object System.Drawing.Size(410, 20)
$helpLabel.Location = New-Object System.Drawing.Point(25, %d)
$helpLabel.Font = $helpFont
$helpLabel.ForeColor = [System.Drawing.Color]::FromArgb(180, 180, 180)
$form.Controls.Add($helpLabel)
`, len(readers), helpY))

	okX := (formWidth - 16 - (btnWidth + 20 + btnWidth)) / 2
	cancelX := okX + btnWidth + 20

	sb.WriteString(fmt.Sprintf(`$okBtn = New-Object System.Windows.Forms.Button
$okBtn.Text = "起動"
$okBtn.Size = New-Object System.Drawing.Size(%d, %d)
$okBtn.Location = New-Object System.Drawing.Point(%d, %d)
$okBtn.Font = $btnFont
$okBtn.FlatStyle = [System.Windows.Forms.FlatStyle]::Flat
$okBtn.BackColor = [System.Drawing.Color]::FromArgb(50, 50, 50)
$okBtn.ForeColor = [System.Drawing.Color]::FromArgb(240, 240, 240)
$okBtn.FlatAppearance.BorderSize = 2
$okBtn.FlatAppearance.BorderColor = [System.Drawing.Color]::FromArgb(100, 100, 100)
$okBtn.DialogResult = [System.Windows.Forms.DialogResult]::OK
$okBtn.Add_GotFocus({
	$this.BackColor = [System.Drawing.Color]::FromArgb(70, 70, 70)
	$this.FlatAppearance.BorderColor = [System.Drawing.Color]::Gold
})
$okBtn.Add_LostFocus({
	$this.BackColor = [System.Drawing.Color]::FromArgb(50, 50, 50)
	$this.FlatAppearance.BorderColor = [System.Drawing.Color]::FromArgb(100, 100, 100)
})
$form.Controls.Add($okBtn)
$form.AcceptButton = $okBtn

$cancelBtn = New-Object System.Windows.Forms.Button
$cancelBtn.Text = "キャンセル"
$cancelBtn.Size = New-Object System.Drawing.Size(%d, %d)
$cancelBtn.Location = New-Object System.Drawing.Point(%d, %d)
$cancelBtn.Font = $btnFont
$cancelBtn.FlatStyle = [System.Windows.Forms.FlatStyle]::Flat
$cancelBtn.BackColor = [System.Drawing.Color]::FromArgb(50, 50, 50)
$cancelBtn.ForeColor = [System.Drawing.Color]::FromArgb(240, 240, 240)
$cancelBtn.FlatAppearance.BorderSize = 2
$cancelBtn.FlatAppearance.BorderColor = [System.Drawing.Color]::FromArgb(100, 100, 100)
$cancelBtn.DialogResult = [System.Windows.Forms.DialogResult]::Cancel
$cancelBtn.Add_GotFocus({
	$this.BackColor = [System.Drawing.Color]::FromArgb(70, 70, 70)
	$this.FlatAppearance.BorderColor = [System.Drawing.Color]::Gold
})
$cancelBtn.Add_LostFocus({
	$this.BackColor = [System.Drawing.Color]::FromArgb(50, 50, 50)
	$this.FlatAppearance.BorderColor = [System.Drawing.Color]::FromArgb(100, 100, 100)
})
$form.Controls.Add($cancelBtn)
$form.CancelButton = $cancelBtn

$form.KeyPreview = $true
$form.Add_KeyDown({
	param($sender, $e)
	$val = -1
	if ($e.KeyCode -ge [System.Windows.Forms.Keys]::D1 -and $e.KeyCode -le [System.Windows.Forms.Keys]::D9) {
		$val = [int]$e.KeyCode - [int][System.Windows.Forms.Keys]::D1
	} elseif ($e.KeyCode -ge [System.Windows.Forms.Keys]::NumPad1 -and $e.KeyCode -le [System.Windows.Forms.Keys]::NumPad9) {
		$val = [int]$e.KeyCode - [int][System.Windows.Forms.Keys]::NumPad1
	}
	if ($val -ge 0 -and $val -lt $rbs.Count) {
		$rbs[$val].Checked = $true
		$rbs[$val].Focus()
	}
})

$form.Add_Shown({
	$form.Activate()
	$rb%d.Focus()
})
[void]$form.ShowDialog()
if ($form.DialogResult -eq [System.Windows.Forms.DialogResult]::OK) {
`, btnWidth, btnHeight, okX, btnY, btnWidth, btnHeight, cancelX, btnY, defaultIdx))

	for i, r := range readers {
		sb.WriteString(fmt.Sprintf("  if ($rb%d.Checked) { Write-Output '%s' }\n", i, r.ID))
	}
	sb.WriteString("} else { Write-Output 'cancel' }\n")

	cmd := newGuiCmd("powershell",
		"-NoProfile",
		"-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encodePSCommand(sb.String()),
	)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	result := strings.TrimSpace(string(out))
	if result == "cancel" || result == "" {
		return ""
	}
	return result
}

// stopScreenReader は指定されたスクリーンリーダーのプロセスを停止します。
// NVDAの場合はまずクリーン終了を試み、停止しない場合に強制終了します。
func stopScreenReader(exe string, timeout time.Duration, readers []Reader) {
	if !isRunning(exe) {
		return
	}
	if strings.EqualFold(exe, NvdaExeName) {
		cleanQuitNvda(readers)
		// クリーン終了を少し待つ
		time.Sleep(500 * time.Millisecond)
		if isRunning(exe) {
			killProcess(exe)
		}
	} else {
		killProcess(exe)
	}
	waitForExit(exe, timeout)
}

// trySwitchWithoutAdmin は管理者権限（UAC）を要求せずにスクリーンリーダーの切り替えを試みます。
// 途中で権限エラーなどによりプロセスの終了や起動に失敗した場合は false を返し、UAC昇格へフォールバックします。
func trySwitchWithoutAdmin(targetID string, cfg Config, readers []Reader) bool {
	// 対象スクリーンリーダーを設定から検索
	var target *Reader
	for i := range readers {
		if readers[i].ID == targetID {
			target = &readers[i]
			break
		}
	}
	if target == nil {
		fallback := map[string]Reader{
			"nvda":     {ID: "nvda", ExeName: NvdaExeName, Path: findNvdaPath()},
			"narrator": {ID: "narrator", ExeName: NarratorExeName, Path: findNarratorPath()},
			"pctalker": {ID: "pctalker", ExeName: PcTalkerExeName, Path: findPcTalkerPath()},
		}
		if r, ok := fallback[targetID]; ok && r.Path != "" {
			target = &r
		}
	}
	if target == nil {
		return false
	}

	// 切り替え開始音声案内 (無音時間中のガイダンス)
	speakText(fmt.Sprintf("%sを起動します。", target.DisplayName))

	// 切り替え開始音 (ピッ: 880Hz, 150ms)
	playBeep(880, 150)

	timeout := time.Duration(cfg.TimeoutSec) * time.Second

	// 起動中のスクリーンリーダーをすべて停止（対象以外）
	for _, exe := range []string{NvdaExeName, NarratorExeName, PcTalkerExeName} {
		if !strings.EqualFold(exe, target.ExeName) && isRunning(exe) {
			stopScreenReader(exe, timeout, readers)
		}
	}

	// すべて停止できたか確認
	for _, exe := range []string{NvdaExeName, NarratorExeName, PcTalkerExeName} {
		if !strings.EqualFold(exe, target.ExeName) && isRunning(exe) {
			// 停止に失敗している場合は管理者権限が必要と判断して false を返す
			return false
		}
	}

	// 起動直前音 (ピピッ: 1000Hz, 100ms を 50ms 空けて 2回)
	playBeep(1000, 100)
	time.Sleep(50 * time.Millisecond)
	playBeep(1000, 100)

	// 対象スクリーンリーダーを起動
	if err := startProcess(target.Path); err != nil {
		return false
	}

	return true
}

// cleanQuitNvda は NVDA のパスを探して nvda.exe -q を実行し、クリーン終了を試みます。
func cleanQuitNvda(readers []Reader) {
	nvdaPath := ""
	for _, r := range readers {
		if r.ID == "nvda" {
			nvdaPath = r.Path
			break
		}
	}
	if nvdaPath == "" {
		nvdaPath = findNvdaPath()
	}
	if nvdaPath != "" {
		_ = newHiddenCmd(nvdaPath, "-q").Run()
	}
}

// runNonAdmin は非管理者として実行される処理（ダイアログ表示 → 非管理者での切替試行 → 失敗時UAC昇格）
func runNonAdmin() {
	cfg := loadConfig()
	readers := buildReaderList(cfg)
	if len(readers) == 0 {
		showError("利用可能なスクリーンリーダーが見つかりませんでした。\nNVDA、ナレーター、PC-Talker のいずれかがインストールされているか確認してください。")
		return
	}

	defaultID := findCurrentReaderID(readers)
	selectedID := showSelectionDialog(readers, defaultID)
	if selectedID == "" {
		return // キャンセル
	}

	// まずは管理者権限を要求せずに切り替えを試みる
	if trySwitchWithoutAdmin(selectedID, cfg, readers) {
		return // 成功した場合は終了
	}

	// 失敗した場合は、選択結果を引数として渡し管理者権限で再起動 → UACが表示される
	if err := runElevated(selectedID); err != nil {
		showError(fmt.Sprintf("管理者権限での起動に失敗しました:\n%v", err))
	}
}

// runAdmin は管理者権限で実行される処理（SRの停止と起動）
func runAdmin(targetID string, cfg Config, readers []Reader) {
	// targetIDが空の場合（引数なし起動）はダイアログを表示
	if targetID == "" {
		defaultID := findCurrentReaderID(readers)
		targetID = showSelectionDialog(readers, defaultID)
		if targetID == "" {
			return
		}
	}

	// 対象スクリーンリーダーを設定から検索
	var target *Reader
	for i := range readers {
		if readers[i].ID == targetID {
			target = &readers[i]
			break
		}
	}
	// 設定リストにない場合でも既知のSRならフォールバック対応
	if target == nil {
		fallback := map[string]Reader{
			"nvda":     {ID: "nvda", ExeName: NvdaExeName, Path: findNvdaPath()},
			"narrator": {ID: "narrator", ExeName: NarratorExeName, Path: findNarratorPath()},
			"pctalker": {ID: "pctalker", ExeName: PcTalkerExeName, Path: findPcTalkerPath()},
		}
		if r, ok := fallback[targetID]; ok && r.Path != "" {
			target = &r
		}
	}
	if target == nil {
		showError(fmt.Sprintf("スクリーンリーダー '%s' が見つかりませんでした。", targetID))
		return
	}

	// 切り替え開始音声案内 (無音時間中のガイダンス)
	speakText(fmt.Sprintf("%sを起動します。", target.DisplayName))

	// 切り替え開始音 (ピッ: 880Hz, 150ms)
	// これにより全盲のユーザーは管理者権限での実行が開始されたことを認識できます
	playBeep(880, 150)

	timeout := time.Duration(cfg.TimeoutSec) * time.Second

	// 起動中のスクリーンリーダーをすべて停止（対象以外）
	for _, exe := range []string{NvdaExeName, NarratorExeName, PcTalkerExeName} {
		if !strings.EqualFold(exe, target.ExeName) && isRunning(exe) {
			stopScreenReader(exe, timeout, readers)
		}
	}

	// 起動直前音 (ピピッ: 1000Hz, 100ms を 50ms 空けて 2回)
	// プロセスの完全終了待機が終わり、新しいスクリーンリーダーを起動する合図です
	playBeep(1000, 100)
	time.Sleep(50 * time.Millisecond)
	playBeep(1000, 100)

	// 対象スクリーンリーダーを起動
	if err := startProcess(target.Path); err != nil {
		showError(fmt.Sprintf("%s の起動に失敗しました:\n%v", target.DisplayName, err))
	}
}

func main() {
	// --- 管理者権限あり: 引数から対象を取得して切り替え実行 ---
	targetID := ""
	args := os.Args[1:]
	for i, a := range args {
		if a == "-reader" && i+1 < len(args) {
			targetID = args[i+1]
			break
		}
	}

	// 引数なしで起動された（＝ダイアログを表示する）場合のみ、多重起動を防止する
	if targetID == "" {
		if !checkSingleInstance() {
			return // 既に起動中なら静かに終了
		}
	}

	// --- 非管理者: ダイアログ表示 → UAC昇格 ---
	if !isAdmin() {
		runNonAdmin()
		return
	}

	cfg := loadConfig()
	readers := buildReaderList(cfg)
	if len(readers) == 0 && targetID == "" {
		showError("利用可能なスクリーンリーダーが見つかりませんでした。")
		return
	}

	runAdmin(targetID, cfg, readers)
}

// waitForExit は指定したプロセスが終了するまで最大 timeout 待ちます
func waitForExit(name string, timeout time.Duration) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			return
		case <-ticker.C:
			if !isRunning(name) {
				return
			}
		}
	}
}

func isAdmin() bool {
	_, err := newHiddenCmd("net", "session").Output()
	return err == nil
}

// copyFile はファイルをコピーします。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// runElevated は選択されたスクリーンリーダーIDを引数に付けて管理者権限で自身を再起動します。
// UAC拒否（ユーザーが「いいえ」を選択）は正常なキャンセル操作として nil を返します。
// スタンドアロン配布（ZIP直下からの実行など）時や、別管理者ユーザーへの昇格時でも正しく動作するよう、
// 実行ファイルをパブリックな一時フォルダにコピーして実行し、終了後に削除します。
func runElevated(readerID string) error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}

	publicDir := os.Getenv("PUBLIC")
	if publicDir == "" {
		publicDir = `C:\Users\Public`
	}

	// プロセスIDを含むユニークな一時ディレクトリを作成
	tempDir := filepath.Join(publicDir, fmt.Sprintf("sr-changer-temp-%d", os.Getpid()))
	if err := os.MkdirAll(tempDir, 0777); err != nil {
		return fmt.Errorf("一時ディレクトリの作成に失敗しました: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// 実行ファイルを一時ディレクトリにコピー
	tempExePath := filepath.Join(tempDir, filepath.Base(exePath))
	if err := copyFile(exePath, tempExePath); err != nil {
		return fmt.Errorf("実行ファイルのコピーに失敗しました: %v", err)
	}

	// 設定ファイル(config.json)が存在すれば、それも一時ディレクトリにコピー
	configSrc := configPath()
	configDst := filepath.Join(tempDir, ConfigFileName)
	if _, err := os.Stat(configSrc); err == nil {
		_ = copyFile(configSrc, configDst) // 設定ファイルのコピー失敗は致命的ではないためエラーは無視
	}

	// PowerShell内シングルクォート文字列では '' でエスケープする
	tempExeEscaped := strings.ReplaceAll(tempExePath, "'", "''")
	// 管理者権限で起動し、その終了を待機する (-Wait オプションを追加)
	script := fmt.Sprintf("Start-Process '%s' -ArgumentList @('-reader','%s') -Verb RunAs -Wait", tempExeEscaped, readerID)
	out, err := newHiddenCmd("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script).CombinedOutput()
	if err != nil {
		msg := strings.ToLower(string(out))
		// UAC拒否はユーザーの正常なキャンセル操作なのでエラー扱いしない
		if strings.Contains(msg, "canceled by the user") || strings.Contains(msg, "cancelled by the user") {
			return nil
		}
		if len(out) > 0 {
			return fmt.Errorf("%s", strings.TrimSpace(string(out)))
		}
		return err
	}
	return nil
}

func isRunning(name string) bool {
	h, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)

	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))

	err = syscall.Process32First(h, &pe)
	if err != nil {
		return false
	}

	target := strings.ToLower(name)
	for {
		var end int
		for end = 0; end < len(pe.ExeFile); end++ {
			if pe.ExeFile[end] == 0 {
				break
			}
		}
		exeName := string(utf16.Decode(pe.ExeFile[:end]))
		if strings.EqualFold(exeName, target) {
			return true
		}

		err = syscall.Process32Next(h, &pe)
		if err != nil {
			break
		}
	}
	return false
}

func startProcess(path string) error {
	return newGuiCmd("cmd", "/C", "start", "", path).Run()
}

func killProcess(name string) {
	h, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	defer syscall.CloseHandle(h)

	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))

	err = syscall.Process32First(h, &pe)
	if err != nil {
		return
	}

	target := strings.ToLower(name)
	for {
		var end int
		for end = 0; end < len(pe.ExeFile); end++ {
			if pe.ExeFile[end] == 0 {
				break
			}
		}
		exeName := string(utf16.Decode(pe.ExeFile[:end]))
		if strings.EqualFold(exeName, target) {
			ph, err := syscall.OpenProcess(PROCESS_TERMINATE, false, pe.ProcessID)
			if err == nil {
				_ = syscall.TerminateProcess(ph, 1)
				syscall.CloseHandle(ph)
			}
		}

		err = syscall.Process32Next(h, &pe)
		if err != nil {
			break
		}
	}
}

var mutexHandle uintptr

var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procBeep         = modkernel32.NewProc("Beep")
	procCreateMutexW = modkernel32.NewProc("CreateMutexW")
)

// checkSingleInstance は多重起動を防止するためにミューテックスを作成します。
// 既に起動している場合は false を返します。
func checkSingleInstance() bool {
	mutexName := "Global\\SRChangerGUI-Mutex-9f3a2b1c"
	namePtr, err := syscall.UTF16PtrFromString(mutexName)
	if err != nil {
		return true // エラー時は安全のため続行
	}
	ret, _, err := procCreateMutexW.Call(0, 1, uintptr(unsafe.Pointer(namePtr)))
	if ret == 0 {
		return true
	}
	if err != nil && err.(syscall.Errno) == 183 { // ERROR_ALREADY_EXISTS
		return false
	}
	mutexHandle = ret
	return true
}

// speakText は Windows 標準の Speech API (SAPI) を非同期で呼び出し、
// スクリーンリーダー切り替え中の無音時間でも音声ガイダンスを提供します。
func speakText(text string) {
	escaped := strings.ReplaceAll(text, "'", "''")
	script := fmt.Sprintf(`(New-Object -ComObject SAPI.SpVoice).Speak('%s', 1)`, escaped)
	_ = newHiddenCmd("powershell",
		"-NoProfile",
		"-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encodePSCommand(script),
	).Start()
}

// playBeep は Windows API の Beep を呼び出して指定周波数・長さのビープ音を鳴らします。
func playBeep(freq, duration uint32) {
	procBeep.Call(uintptr(freq), uintptr(duration))
}

// newHiddenCmd は Windows でコンソールウィンドウが表示されないように設定した exec.Cmd を作成します。
func newHiddenCmd(name string, arg ...string) *exec.Cmd {
	cmd := exec.Command(name, arg...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	return cmd
}

// newGuiCmd は Windows でコンソールウィンドウは非表示のまま、
// 子プロセスが作成する GUI ウィンドウ（WinForms等）は通常通り表示できるようにした exec.Cmd を作成します。
func newGuiCmd(name string, arg ...string) *exec.Cmd {
	cmd := exec.Command(name, arg...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    false,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	return cmd
}