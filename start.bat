@echo off
chcp 65001 >nul 2>&1
title GameTunnel

:: Request admin rights
net session >nul 2>&1
if %errorlevel% neq 0 (
    echo Requesting admin rights...
    powershell -Command "Start-Process '%~f0' -Verb RunAs"
    exit /b
)

:: Find client executable
set EXE=gtunnel-client.exe
where %EXE% >nul 2>&1
if %errorlevel% neq 0 (
    if exist "%LOCALAPPDATA%\GameTunnel\gtunnel-client.exe" (
        set EXE=%LOCALAPPDATA%\GameTunnel\gtunnel-client.exe
    ) else if exist "%~dp0gtunnel-client.exe" (
        set EXE=%~dp0gtunnel-client.exe
    ) else (
        echo [ERROR] Cannot find gtunnel-client.exe
        echo Make sure it is in the current directory, PATH, or %LOCALAPPDATA%\GameTunnel
        echo Download: https://github.com/holipay/gametunnel/releases
        pause
        exit /b 1
    )
)

:: Read config from config.json
set CONFIG_FILE=%APPDATA%\GameTunnel\config.json
if not exist "%CONFIG_FILE%" (
    echo [ERROR] Config file not found: %CONFIG_FILE%
    echo Please run the installer first, or create config manually
    echo Format: {"server_addr":"IP:4700","player_name":"Name","room_id":"default","room_password":""}
    pause
    exit /b 1
)

for /f "usebackq delims=" %%i in (`powershell -NoProfile -Command ^
    "$c = Get-Content '%CONFIG_FILE%' | ConvertFrom-Json; ^
    Write-Host ($c.server_addr + '|' + $c.player_name + '|' + $c.room_id + '|' + $c.room_password)"`) do (
    for /f "tokens=1-4 delims=|" %%a in ("%%i") do (
        set SERVER=%%a
        set NAME=%%b
        set ROOM=%%c
        set PASSWORD=%%d
    )
)

:: Build arguments
set ARGS=-server %SERVER%
if defined NAME if not "%NAME%"=="" set ARGS=%ARGS% -name "%NAME%"
if not "%ROOM%"=="default" set ARGS=%ARGS% -room %ROOM%
if defined PASSWORD if not "%PASSWORD%"=="" set ARGS=%ARGS% -password "%PASSWORD%"

echo.
echo  ========================================
echo   GameTunnel - LAN Game Tunnel
echo  ========================================
echo   Server: %SERVER%
if defined NAME if not "%NAME%"=="" echo   Player: %NAME%
echo   Room:   %ROOM%
if defined PASSWORD if not "%PASSWORD%"=="" (echo   Auth:   HMAC) else (echo   Auth:   None)
echo  ========================================
echo.

:: Launch
"%EXE%" %ARGS%

echo.
echo GameTunnel exited.
pause
