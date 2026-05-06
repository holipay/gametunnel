# GameTunnel Client Installer (Windows PowerShell)
#
# One-liner install (interactive):
#   irm https://raw.githubusercontent.com/holipay/gametunnel/main/install-client.ps1 | iex
#
# One-liner install (with server):
#   $env:GT_SERVER="1.2.3.4:4700"; irm https://raw.githubusercontent.com/holipay/gametunnel/main/install-client.ps1 | iex
#
# Direct install:
#   .\install-client.ps1 -Server 1.2.3.4:4700
#   .\install-client.ps1 -Server 1.2.3.4:4700 -Name Player1 -Room myroom -Password secret
#
# Offline install:
#   Place gtunnel-client.exe, wintun.dll and install-client.ps1 in the same folder, then run.

function Install-GameTunnel {
    param(
        [string]$Server = $env:GT_SERVER,
        [string]$Name = "",
        [string]$Room = "default",
        [string]$Password = ""
    )

    $ErrorActionPreference = "Stop"

    $InstallDir = "$env:LOCALAPPDATA\GameTunnel"
    $ExeUrl = "https://github.com/holipay/gametunnel/releases/latest/download/gtunnel-client.exe"
    $ExePath = "$InstallDir\gtunnel-client.exe"

    # Default player name to computer name
    if (-not $Name) { $Name = $env:COMPUTERNAME }

    Write-Host ""
    Write-Host "  ========================================" -ForegroundColor Cyan
    Write-Host "   GameTunnel - LAN Game Tunnel" -ForegroundColor Cyan
    Write-Host "  ========================================" -ForegroundColor Cyan
    Write-Host ""

    # Interactive prompt if no server specified
    if (-not $Server) {
        $Server = Read-Host "  Server address (IP:4700)"
        if (-not $Server) {
            Write-Host "  [ERROR] Server address is required" -ForegroundColor Red
            return
        }
    }

    # Check admin rights
    $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    if (-not $isAdmin) {
        Write-Host "  [WARN] Not running as admin. Client needs admin on first run." -ForegroundColor Yellow
        Write-Host ""
    }

    # Create install directory
    if (!(Test-Path $InstallDir)) {
        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    }

    # ── Get gtunnel-client.exe ──────────────────────────────────
    $ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
    if (-not $ScriptDir) { $ScriptDir = "." }
    $LocalExe = Join-Path $ScriptDir "gtunnel-client.exe"

    if ((Test-Path $LocalExe) -and (Get-Item $LocalExe).Length -gt 100000) {
        Write-Host "  [OK] gtunnel-client.exe (local)" -ForegroundColor Green
        Copy-Item -Path $LocalExe -Destination $ExePath -Force
    } elseif (Test-Path $ExePath) {
        Write-Host "  [OK] gtunnel-client.exe (installed)" -ForegroundColor Green
    } else {
        Write-Host "  [..] Downloading gtunnel-client.exe..." -ForegroundColor Yellow
        try {
            [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
            Invoke-WebRequest -Uri $ExeUrl -OutFile $ExePath -UseBasicParsing
        } catch {
            Write-Host "  [FAIL] Download failed: $_" -ForegroundColor Red
            Write-Host "  Download manually from https://github.com/holipay/gametunnel/releases" -ForegroundColor Yellow
            return
        }
        if (!(Test-Path $ExePath) -or (Get-Item $ExePath).Length -lt 100000) {
            Write-Host "  [FAIL] Downloaded file is invalid" -ForegroundColor Red
            return
        }
        Write-Host "  [OK] gtunnel-client.exe" -ForegroundColor Green
    }

    # ── Get wintun.dll ──────────────────────────────────────────
    $WintunPath = "$InstallDir\wintun.dll"
    $LocalWintun = Join-Path $ScriptDir "wintun.dll"

    if (Test-Path $WintunPath) {
        Write-Host "  [OK] wintun.dll (installed)" -ForegroundColor Green
    } elseif ((Test-Path $LocalWintun) -and (Get-Item $LocalWintun).Length -gt 10000) {
        Write-Host "  [OK] wintun.dll (local)" -ForegroundColor Green
        Copy-Item -Path $LocalWintun -Destination $WintunPath -Force
    } else {
        Write-Host "  [..] Downloading wintun.dll..." -ForegroundColor Yellow
        $WintunZip = "$env:TEMP\wintun.zip"
        $WintunZipUrl = "https://www.wintun.net/builds/wintun-0.14.1.zip"
        try {
            [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
            Invoke-WebRequest -Uri $WintunZipUrl -OutFile $WintunZip -UseBasicParsing
            Add-Type -AssemblyName System.IO.Compression.FileSystem
            $arch = if ([Environment]::Is64BitOperatingSystem) { "amd64" } else { "x86" }
            $zip = [IO.Compression.ZipFile]::OpenRead($WintunZip)
            foreach ($entry in $zip.Entries) {
                if ($entry.FullName -like "*$arch*wintun.dll") {
                    [IO.Compression.ZipFileExtensions]::ExtractToFile($entry, $WintunPath, $true)
                    break
                }
            }
            $zip.Dispose()
        } catch {
            Write-Host "  [WARN] wintun.dll download failed: $_" -ForegroundColor Yellow
        } finally {
            Remove-Item -Path $WintunZip -ErrorAction SilentlyContinue
        }
        if ((Test-Path $WintunPath) -and (Get-Item $WintunPath).Length -gt 10000) {
            Write-Host "  [OK] wintun.dll" -ForegroundColor Green
        } else {
            Write-Host "  [WARN] wintun.dll missing, virtual NIC may fail" -ForegroundColor Yellow
        }
    }

    # ── Add to PATH ─────────────────────────────────────────────
    $currentPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if ($currentPath -notlike "*$InstallDir*") {
        [Environment]::SetEnvironmentVariable("Path", "$currentPath;$InstallDir", "User")
        $env:Path += ";$InstallDir"
        Write-Host "  [OK] Added to PATH" -ForegroundColor Green
    }

    # ── Write config ────────────────────────────────────────────
    $ConfigDir = "$env:APPDATA\GameTunnel"
    if (!(Test-Path $ConfigDir)) {
        New-Item -ItemType Directory -Path $ConfigDir -Force | Out-Null
    }
    $Config = @{
        server_addr   = $Server
        player_name   = $Name
        room_id       = $Room
        room_password = $Password
    } | ConvertTo-Json
    Set-Content -Path "$ConfigDir\config.json" -Value $Config -Encoding UTF8
    Write-Host "  [OK] Config saved" -ForegroundColor Green

    # ── Create desktop shortcut ─────────────────────────────────
    $BatPath = "$InstallDir\start.bat"
    $BatContent = @'
@echo off
chcp 65001 >nul 2>&1
title GameTunnel
net session >nul 2>&1
if %errorlevel% neq 0 (
    powershell -Command "Start-Process '%~f0' -Verb RunAs"
    exit /b
)
set EXE=%LOCALAPPDATA%\GameTunnel\gtunnel-client.exe
if not exist "%EXE%" (echo [ERROR] Cannot find %EXE% & pause & exit /b 1)
for /f "usebackq delims=" %%i in (`powershell -NoProfile -Command "$c = Get-Content '%APPDATA%\GameTunnel\config.json' | ConvertFrom-Json; Write-Host ($c.server_addr + '|' + $c.player_name + '|' + $c.room_id + '|' + $c.room_password)"`) do (
    for /f "tokens=1-4 delims=|" %%a in ("%%i") do (set SERVER=%%a & set NAME=%%b & set ROOM=%%c & set PASS=%%d)
)
set ARGS=-server %SERVER%
if defined NAME if not "%NAME%"=="" set ARGS=%ARGS% -name "%NAME%"
if defined ROOM if not "%ROOM%"=="default" set ARGS=%ARGS% -room %ROOM%
if defined PASS if not "%PASS%"=="" set ARGS=%ARGS% -password "%PASS%"
"%EXE%" %ARGS%
pause
'@
    Set-Content -Path $BatPath -Value $BatContent -Encoding ASCII

    try {
        $WshShell = New-Object -ComObject WScript.Shell
        $Shortcut = $WshShell.CreateShortcut("$env:USERPROFILE\Desktop\GameTunnel.lnk")
        $Shortcut.TargetPath = $BatPath
        $Shortcut.WorkingDirectory = $InstallDir
        $Shortcut.Description = "GameTunnel - LAN Game Tunnel"
        $Shortcut.Save()
        Write-Host "  [OK] Desktop shortcut created" -ForegroundColor Green
    } catch {
        Write-Host "  [WARN] Could not create desktop shortcut" -ForegroundColor Yellow
    }

    # ── Done ────────────────────────────────────────────────────
    Write-Host ""
    Write-Host "  ========================================" -ForegroundColor Green
    Write-Host "   Installation complete!" -ForegroundColor Green
    Write-Host "  ========================================" -ForegroundColor Green
    Write-Host ""
    Write-Host "  Server: $Server" -ForegroundColor White
    Write-Host "  Player: $Name" -ForegroundColor White
    Write-Host "  Room:   $Room" -ForegroundColor White
    Write-Host ""
    Write-Host "  To start:" -ForegroundColor White
    Write-Host "    Double-click GameTunnel on Desktop" -ForegroundColor White
    Write-Host "    Or run: $InstallDir\start.bat" -ForegroundColor White
    Write-Host ""
    Write-Host "  After connecting, open your game and enter LAN mode." -ForegroundColor White
    Write-Host ""
}

# Execute when piped to iex or run directly
Install-GameTunnel @args
