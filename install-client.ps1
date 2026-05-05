# GameTunnel Client Installer (Windows PowerShell)
#
# Usage:
#   Method 1: Remote install
#     irm https://raw.githubusercontent.com/holipay/gametunnel/main/install-client.ps1 -OutFile install.ps1
#     .\install.ps1 -Server 111.229.82.204
#
#   Method 2: With parameters
#     .\install-client.ps1 -Server 111.229.82.204 -Password mypass
#
#   Method 3: Offline install
#     Place gtunnel-client.exe, wintun.dll and install-client.ps1 in the same folder
#
# File priority: local files in script folder -> download from network

param(
    [Parameter(Mandatory=$true)]
    [string]$Server,
    [string]$Name = $env:COMPUTERNAME,
    [string]$Room = "default",
    [string]$Password = ""
)

$ErrorActionPreference = "Stop"

$InstallDir = "$env:LOCALAPPDATA\GameTunnel"
$ExeUrl = "https://github.com/holipay/gametunnel/releases/latest/download/gtunnel-client.exe"
$ExePath = "$InstallDir\gtunnel-client.exe"

Write-Host ""
Write-Host "  GameTunnel Client Installer" -ForegroundColor Cyan
Write-Host ""

# Check admin rights (not required for install, but required for first run)
$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    Write-Host "  [WARN] Not running as admin. Install can continue, but client needs admin on first run." -ForegroundColor Yellow
    Write-Host ""
}

# Create install directory
if (!(Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
}

# Get client: prefer local file in script folder, fallback to GitHub download
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$LocalExe = Join-Path $ScriptDir "gtunnel-client.exe"
if ((Test-Path $LocalExe) -and (Get-Item $LocalExe).Length -gt 100000) {
    Write-Host "  [OK] Using local file: $LocalExe" -ForegroundColor Green
    Copy-Item -Path $LocalExe -Destination $ExePath -Force
} else {
    Write-Host "  [..] Downloading gtunnel-client.exe from GitHub..." -ForegroundColor Yellow
    try {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        Invoke-WebRequest -Uri $ExeUrl -OutFile $ExePath -UseBasicParsing
    } catch {
        Write-Host "  [FAIL] Download failed: $_" -ForegroundColor Red
        Write-Host "  Please download manually from https://github.com/holipay/gametunnel/releases" -ForegroundColor Yellow
        exit 1
    }
}

# Verify client file
if (!(Test-Path $ExePath) -or (Get-Item $ExePath).Length -lt 100000) {
    Write-Host "  [FAIL] Client file is invalid (blocked or incomplete download)" -ForegroundColor Red
    exit 1
}
Write-Host "  [OK] Client ready: $ExePath" -ForegroundColor Green

# Get wintun.dll: prefer local, fallback to download from wintun.net
$WintunPath = "$InstallDir\wintun.dll"
$LocalWintun = Join-Path $ScriptDir "wintun.dll"
$WintunZipUrl = "https://www.wintun.net/builds/wintun-0.14.1.zip"

if (Test-Path $WintunPath) {
    Write-Host "  [OK] wintun.dll already exists" -ForegroundColor Green
} elseif ((Test-Path $LocalWintun) -and (Get-Item $LocalWintun).Length -gt 10000) {
    Write-Host "  [OK] Using local wintun.dll: $LocalWintun" -ForegroundColor Green
    Copy-Item -Path $LocalWintun -Destination $WintunPath -Force
} else {
    Write-Host "  [..] Downloading wintun.dll..." -ForegroundColor Yellow
    $WintunZip = "$env:TEMP\wintun.zip"
    try {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        Invoke-WebRequest -Uri $WintunZipUrl -OutFile $WintunZip -UseBasicParsing
        # Determine architecture
        $Arch = if ([Environment]::Is64BitOperatingSystem) { "amd64" } else { "x86" }
        # Extract wintun.dll from zip
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        $zip = [IO.Compression.ZipFile]::OpenRead($WintunZip)
        foreach ($entry in $zip.Entries) {
            if ($entry.FullName -like "*$Arch*wintun.dll") {
                [IO.Compression.ZipFileExtensions]::ExtractToFile($entry, $WintunPath, $true)
                break
            }
        }
        $zip.Dispose()
    } catch {
        Write-Host "  [WARN] wintun.dll download failed: $_" -ForegroundColor Yellow
        Write-Host "  Please download manually from https://www.wintun.net/" -ForegroundColor Yellow
    } finally {
        Remove-Item -Path $WintunZip -ErrorAction SilentlyContinue
    }
}

if ((Test-Path $WintunPath) -and (Get-Item $WintunPath).Length -gt 10000) {
    Write-Host "  [OK] wintun.dll ready" -ForegroundColor Green
} else {
    Write-Host "  [WARN] wintun.dll missing, client may fail to create virtual NIC" -ForegroundColor Yellow
}

# Add to PATH (current user)
$currentPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($currentPath -notlike "*$InstallDir*") {
    [Environment]::SetEnvironmentVariable("Path", "$currentPath;$InstallDir", "User")
    $env:Path += ";$InstallDir"
    Write-Host "  [OK] Added to PATH" -ForegroundColor Green
}

# Write config file
$ConfigDir = "$env:APPDATA\GameTunnel"
if (!(Test-Path $ConfigDir)) {
    New-Item -ItemType Directory -Path $ConfigDir -Force | Out-Null
}
$Config = @{
    server_addr   = "${Server}:4700"
    player_name   = $Name
    room_id       = $Room
    room_password = $Password
} | ConvertTo-Json
Set-Content -Path "$ConfigDir\config.json" -Value $Config -Encoding UTF8
Write-Host "  [OK] Config written: $ConfigDir\config.json" -ForegroundColor Green

# Generate start.bat (reads config from config.json, no hardcoded values)
$BatContent = @'
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

set EXE=%LOCALAPPDATA%\GameTunnel\gtunnel-client.exe
if not exist "%EXE%" (
    echo [ERROR] Cannot find %EXE%
    echo Please re-run the installer
    pause
    exit /b 1
)

:: Read config from config.json via PowerShell
for /f "usebackq delims=" %%i in (`powershell -NoProfile -Command ^
    "$c = Get-Content '%APPDATA%\GameTunnel\config.json' | ConvertFrom-Json; ^
    Write-Host ($c.server_addr + '|' + $c.player_name + '|' + $c.room_id + '|' + $c.room_password)"`) do (
    for /f "tokens=1-4 delims=|" %%a in ("%%i") do (
        set SERVER=%%a
        set NAME=%%b
        set ROOM=%%c
        set PASSWORD=%%d
    )
)

echo.
echo  ========================================
echo   GameTunnel - LAN Game Tunnel
echo  ========================================
echo   Server: %SERVER%
if defined NAME echo   Player: %NAME%
echo   Room:   %ROOM%
if defined PASSWORD if not "%PASSWORD%"=="" (echo   Auth:   HMAC) else (echo   Auth:   None)
echo  ========================================
echo.

set ARGS=-server %SERVER%
if defined NAME if not "%NAME%"=="" set ARGS=%ARGS% -name "%NAME%"
if not "%ROOM%"=="default" set ARGS=%ARGS% -room %ROOM%
if defined PASSWORD if not "%PASSWORD%"=="" set ARGS=%ARGS% -password "%PASSWORD%"

"%EXE%" %ARGS%

echo.
echo GameTunnel exited.
pause
'@
Set-Content -Path "$InstallDir\start.bat" -Value $BatContent -Encoding ASCII
Write-Host "  [OK] Launcher created: $InstallDir\start.bat" -ForegroundColor Green

# Create desktop shortcut
$WshShell = New-Object -ComObject WScript.Shell
$Shortcut = $WshShell.CreateShortcut("$env:USERPROFILE\Desktop\GameTunnel.lnk")
$Shortcut.TargetPath = "$InstallDir\start.bat"
$Shortcut.WorkingDirectory = $InstallDir
$Shortcut.Description = "GameTunnel - LAN Game Tunnel"
$Shortcut.Save()
Write-Host "  [OK] Desktop shortcut created" -ForegroundColor Green

Write-Host ""
Write-Host "  Installation complete!" -ForegroundColor Green
Write-Host ""
Write-Host "  How to start:" -ForegroundColor White
Write-Host "    1. Double-click GameTunnel shortcut on Desktop" -ForegroundColor White
Write-Host "    2. Run $InstallDir\start.bat" -ForegroundColor White
Write-Host "    3. Command line:" -ForegroundColor White
Write-Host "       gtunnel-client.exe -server ${Server}:4700" -ForegroundColor Cyan
Write-Host ""
Write-Host "  After connecting, open your game and enter LAN mode." -ForegroundColor White
Write-Host ""
Write-Host "  [WARN] First run needs admin rights (to create wintun virtual NIC)" -ForegroundColor Yellow
Write-Host "  [WARN] Make sure Windows Firewall allows gtunnel-client.exe" -ForegroundColor Yellow
Write-Host ""
