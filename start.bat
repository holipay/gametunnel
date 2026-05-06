@echo off
chcp 65001 >nul 2>&1
title GameTunnel

:: ── Request admin rights ──────────────────────────────────────
net session >nul 2>&1
if %errorlevel% neq 0 (
    echo  Requesting admin rights...
    powershell -Command "Start-Process '%~f0' -Verb RunAs"
    exit /b
)

:: ── Paths ─────────────────────────────────────────────────────
set "INSTALL_DIR=%LOCALAPPDATA%\GameTunnel"
set "EXE=%INSTALL_DIR%\gtunnel-client.exe"
set "WINTUN=%INSTALL_DIR%\wintun.dll"
set "CONFIG=%APPDATA%\GameTunnel\config.json"
set "REPO=https://github.com/holipay/gametunnel/releases/latest/download"

:: Portable mode: files next to this script
set "SCRIPT_DIR=%~dp0"
set "LOCAL_EXE=%SCRIPT_DIR%gtunnel-client.exe"
set "LOCAL_WINTUN=%SCRIPT_DIR%wintun.dll"

:: ── Banner ────────────────────────────────────────────────────
echo.
echo  ========================================
echo   GameTunnel - LAN Game Tunnel
echo  ========================================
echo.

:: ── Ensure install directory ──────────────────────────────────
if not exist "%INSTALL_DIR%" mkdir "%INSTALL_DIR%"

:: ── Find or download gtunnel-client.exe ───────────────────────
if exist "%LOCAL_EXE%" (
    echo  [OK] gtunnel-client.exe (local)
    copy /y "%LOCAL_EXE%" "%EXE%" >nul 2>&1
) else if exist "%EXE%" (
    echo  [OK] gtunnel-client.exe (installed)
) else (
    echo  [..] Downloading gtunnel-client.exe...
    call :download "%REPO%/gtunnel-client.exe" "%EXE%"
    if errorlevel 1 (
        echo  [FAIL] Download failed. Please download manually:
        echo         https://github.com/holipay/gametunnel/releases
        echo.
        pause
        exit /b 1
    )
    echo  [OK] gtunnel-client.exe
)

:: ── Find or download wintun.dll ───────────────────────────────
if exist "%LOCAL_WINTUN%" (
    echo  [OK] wintun.dll (local)
    copy /y "%LOCAL_WINTUN%" "%WINTUN%" >nul 2>&1
) else if exist "%WINTUN%" (
    echo  [OK] wintun.dll (installed)
) else (
    echo  [..] Downloading wintun.dll...
    :: Try GitHub release first
    call :download "%REPO%/wintun.dll" "%WINTUN%"
    if not exist "%WINTUN%" (
        :: Fallback: download zip from wintun.net and extract
        call :download_wintun_from_zip
    )
    if exist "%WINTUN%" (
        echo  [OK] wintun.dll
    ) else (
        echo  [WARN] wintun.dll missing, virtual NIC may fail to create
        echo         Download manually from https://www.wintun.net/
    )
)

:: ── Config wizard (first run) ─────────────────────────────────
if not exist "%CONFIG%" (
    echo.
    echo  First-time setup
    echo  ----------------------------------------
    echo.
    set /p "SERVER=  Server address (IP:4700): "
    if "!SERVER!"=="" (
        echo  [ERROR] Server address is required
        pause
        exit /b 1
    )
    set /p "PNAME=  Player name [%COMPUTERNAME%]: "
    if "!PNAME!"=="" set "PNAME=%COMPUTERNAME%"
    set /p "ROOM=  Room ID [default]: "
    if "!ROOM!"=="" set "ROOM=default"
    set /p "PASS=  Password (leave empty if none): "

    :: Create config
    if not exist "%APPDATA%\GameTunnel" mkdir "%APPDATA%\GameTunnel"
    powershell -NoProfile -Command ^
        "$c = @{server_addr='%SERVER%'; player_name='%PNAME%'; room_id='%ROOM%'; room_password='%PASS%'} | ConvertTo-Json; [IO.File]::WriteAllText('%CONFIG%', $c, [Text.Encoding]::UTF8)"

    echo  [OK] Config saved to %CONFIG%
    echo.
)

:: ── Read config ───────────────────────────────────────────────
for /f "usebackq delims=" %%i in (`powershell -NoProfile -Command ^
    "$c = Get-Content '%CONFIG%' | ConvertFrom-Json; ^
    Write-Host ($c.server_addr + '|' + $c.player_name + '|' + $c.room_id + '|' + $c.room_password)"`) do (
    for /f "tokens=1-4 delims=|" %%a in ("%%i") do (
        set "SERVER=%%a"
        set "PNAME=%%b"
        set "ROOM=%%c"
        set "PASS=%%d"
    )
)

:: ── Display info ──────────────────────────────────────────────
echo  Server: %SERVER%
if defined PNAME if not "%PNAME%"=="" echo  Player: %PNAME%
echo  Room:   %ROOM%
if defined PASS if not "%PASS%"=="" (echo  Auth:   HMAC) else (echo  Auth:   None)
echo  ----------------------------------------
echo.

:: ── Build args and launch ─────────────────────────────────────
set "ARGS=-server %SERVER%"
if defined PNAME if not "%PNAME%"=="" set "ARGS=%ARGS% -name "%PNAME%""
if defined ROOM if not "%ROOM%"=="default" set "ARGS=%ARGS% -room %ROOM%"
if defined PASS if not "%PASS%"=="" set "ARGS=%ARGS% -password "%PASS%""

"%EXE%" %ARGS%

echo.
echo  GameTunnel disconnected.
pause
exit /b

:: ═══════════════════════════════════════════════════════════════
:: Functions
:: ═══════════════════════════════════════════════════════════════

:download
powershell -NoProfile -Command "[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12; try { Invoke-WebRequest -Uri '%~1' -OutFile '%~2' -UseBasicParsing } catch { exit 1 }" 2>nul
if not exist "%~2" exit /b 1
for %%A in ("%~2") do if %%~zA LSS 1000 (del "%~2" 2>nul & exit /b 1)
exit /b 0

:download_wintun_from_zip
set "ZFILE=%TEMP%\wintun.zip"
call :download "https://www.wintun.net/builds/wintun-0.14.1.zip" "%ZFILE%"
if not exist "%ZFILE%" exit /b 1
powershell -NoProfile -Command ^
    "Add-Type -AssemblyName System.IO.Compression.FileSystem; ^
     $arch = if ([Environment]::Is64BitOperatingSystem) { 'amd64' } else { 'x86' }; ^
     $zip = [IO.Compression.ZipFile]::OpenRead('%ZFILE%'); ^
     foreach ($e in $zip.Entries) { ^
         if ($e.FullName -like \"*$arch*wintun.dll\") { ^
             [IO.Compression.ZipFileExtensions]::ExtractToFile($e, '%WINTUN%', $true); break ^
         } ^
     }; ^
     $zip.Dispose()"
del "%ZFILE%" 2>nul
exit /b 0
