@echo off
chcp 65001 >nul 2>&1
title GameTunnel
setlocal enabledelayedexpansion

:: Request admin rights
net session >nul 2>&1
if %errorlevel% neq 0 (
    powershell -Command "Start-Process '%~f0' -Verb RunAs"
    exit /b
)

:: Paths
set "INSTALL_DIR=%LOCALAPPDATA%\GameTunnel"
set "EXE=%INSTALL_DIR%\gtunnel-client.exe"
set "WINTUN=%INSTALL_DIR%\wintun.dll"
set "CONFIG=%APPDATA%\GameTunnel\config.json"
set "REPO=https://github.com/holipay/gametunnel/releases/latest/download"
set "SCRIPT_DIR=%~dp0"

echo.
echo  ========================================
echo   GameTunnel - LAN Game Tunnel
echo  ========================================
echo.

if not exist "%INSTALL_DIR%" mkdir "%INSTALL_DIR%"

:: ── gtunnel-client.exe ────────────────────────────────────────

if not exist "%SCRIPT_DIR%gtunnel-client.exe" goto :check_exe_installed
echo  [OK] gtunnel-client.exe - local
copy /y "%SCRIPT_DIR%gtunnel-client.exe" "%EXE%" >nul 2>&1
goto :check_wintun

:check_exe_installed
if not exist "%EXE%" goto :download_exe
echo  [OK] gtunnel-client.exe - installed
goto :check_wintun

:download_exe
echo  [..] Downloading gtunnel-client.exe...
call :download "%REPO%/gtunnel-client.exe" "%EXE%"
if errorlevel 1 (
    echo  [FAIL] Download failed.
    echo  Please download from https://github.com/holipay/gametunnel/releases
    pause
    exit /b 1
)
echo  [OK] gtunnel-client.exe

:: ── wintun.dll ────────────────────────────────────────────────

:check_wintun
if not exist "%SCRIPT_DIR%wintun.dll" goto :check_wintun_installed
echo  [OK] wintun.dll - local
copy /y "%SCRIPT_DIR%wintun.dll" "%WINTUN%" >nul 2>&1
goto :check_config

:check_wintun_installed
if not exist "%WINTUN%" goto :download_wintun
echo  [OK] wintun.dll - installed
goto :check_config

:download_wintun
echo  [..] Downloading wintun.dll...
call :download "%REPO%/wintun.dll" "%WINTUN%"
if exist "%WINTUN%" goto :wintun_ok
call :download_wintun_from_zip
if not exist "%WINTUN%" goto :wintun_fail

:wintun_ok
echo  [OK] wintun.dll
goto :check_config

:wintun_fail
echo  [WARN] wintun.dll missing, virtual NIC may fail
echo  Download manually from https://www.wintun.net/

:: ── Config wizard ─────────────────────────────────────────────

:check_config
if exist "%CONFIG%" goto :read_config

echo.
echo  First-time setup
echo  ----------------------------------------
echo.
set /p "SERVER=  Server address - IP:4700: "
if "!SERVER!"=="" (
    echo  [ERROR] Server address is required
    pause
    exit /b 1
)
set /p "PNAME=  Player name - default %COMPUTERNAME%: "
if "!PNAME!"=="" set "PNAME=%COMPUTERNAME%"
set /p "ROOM=  Room ID - default: "
if "!ROOM!"=="" set "ROOM=default"
set /p "PASS=  Password - leave empty if none: "

if not exist "%APPDATA%\GameTunnel" mkdir "%APPDATA%\GameTunnel"
powershell -NoProfile -Command "$c=@{server_addr='!SERVER!';player_name='!PNAME!';room_id='!ROOM!';room_password='!PASS!'}|ConvertTo-Json;[IO.File]::WriteAllText('%CONFIG%',$c,[Text.Encoding]::UTF8)"
echo  [OK] Config saved
echo.

:: ── Read config ───────────────────────────────────────────────

:read_config
for /f "usebackq delims=" %%i in (`powershell -NoProfile -Command "$c=Get-Content '%CONFIG%'|ConvertFrom-Json;Write-Host($c.server_addr+'|'+$c.player_name+'|'+$c.room_id+'|'+$c.room_password)"`) do (
    for /f "tokens=1-4 delims=|" %%a in ("%%i") do (
        set "SERVER=%%a"
        set "PNAME=%%b"
        set "ROOM=%%c"
        set "PASS=%%d"
    )
)

:: ── Display info ──────────────────────────────────────────────

echo  Server: !SERVER!
if defined PNAME echo  Player: !PNAME!
echo  Room:   !ROOM!
if defined PASS if not "!PASS!"=="" (
    echo  Auth:   HMAC
) else (
    echo  Auth:   None
)
echo  ----------------------------------------
echo.

:: ── Launch ────────────────────────────────────────────────────

set "ARGS=-server !SERVER!"
if defined PNAME if not "!PNAME!"=="" set "ARGS=!ARGS! -name !PNAME!"
if defined ROOM if not "!ROOM!"=="default" set "ARGS=!ARGS! -room !ROOM!"
if defined PASS if not "!PASS!"=="" set "ARGS=!ARGS! -password !PASS!"

"!EXE!" !ARGS!

echo.
echo  GameTunnel disconnected.
pause
exit /b

:: ═══════════════════════════════════════════════════════════════
:: Functions
:: ═══════════════════════════════════════════════════════════════

:download
powershell -NoProfile -Command "[Net.ServicePointManager]::SecurityProtocol=[Net.SecurityProtocolType]::Tls12;try{Invoke-WebRequest -Uri '%~1' -OutFile '%~2' -UseBasicParsing}catch{exit 1}" 2>nul
if not exist "%~2" exit /b 1
for %%A in ("%~2") do if %%~zA LSS 1000 (del "%~2" 2>nul & exit /b 1)
exit /b 0

:download_wintun_from_zip
set "ZFILE=%TEMP%\wintun.zip"
call :download "https://www.wintun.net/builds/wintun-0.14.1.zip" "%ZFILE%"
if not exist "%ZFILE%" exit /b 1
powershell -NoProfile -Command "Add-Type -AssemblyName System.IO.Compression.FileSystem;$arch=if([Environment]::Is64BitOperatingSystem){'amd64'}else{'x86'};$zip=[IO.Compression.ZipFile]::OpenRead('%ZFILE%');foreach($e in $zip.Entries){if($e.FullName -like '*$arch*wintun.dll'){[IO.Compression.ZipFileExtensions]::ExtractToFile($e,'%WINTUN%',$true);break}};$zip.Dispose()"
del "%ZFILE%" 2>nul
exit /b 0
