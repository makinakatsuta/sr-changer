@echo off
echo Building sr-changer.exe...
go build -ldflags="-s -w -H windowsgui" -o sr-changer.exe main.go
if %errorlevel% neq 0 (
    echo Build failed!
    pause
    exit /b %errorlevel%
)

echo Build completed successfully!
pause
