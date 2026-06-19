@echo off
echo Building sr-changer.exe...
go build -ldflags="-s -w -H windowsgui" -o sr-changer.exe main.go
if %errorlevel% neq 0 (
    echo Build failed!
    pause
    exit /b %errorlevel%
)

echo Packaging sr-changer.zip...
powershell -Command "if (Test-Path sr-changer.zip) { Remove-Item sr-changer.zip }; Compress-Archive -Path sr-changer.exe, README.txt, README.md, LICENSE -DestinationPath sr-changer.zip -Force"
if %errorlevel% neq 0 (
    echo Packaging failed!
    pause
    exit /b %errorlevel%
)

echo Build and packaging completed successfully!
pause
