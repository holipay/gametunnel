@echo off
chcp 65001 >nul 2>&1
title GameTunnel
setlocal enabledelayedexpansion

:: Request admin rights (via VBScript, no PowerShell needed)
net session >nul 2>&1
if %errorlevel% equ 0 goto :run
echo Set UAC = CreateObject^("Shell.Application"^) > "%temp%\gt_elevate.vbs"
echo UAC.ShellExecute "cmd.exe", "/c ""%~f0""", "", "runas", 1 >> "%temp%\gt_elevate.vbs"
cscript //nologo "%temp%\gt_elevate.vbs"
del "%temp%\gt_elevate.vbs" 2>nul
exit /b

:run
set "EXE=%~dp0gtunnel-client.exe"
if not exist "!EXE!" (
    echo.
    echo  [ERROR] gtunnel-client.exe not found
    echo  Please extract all files from the zip to the same folder.
    echo.
    pause
    exit /b 1
)

cd /d "%~dp0"
"!EXE!"
echo.
pause
