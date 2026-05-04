# GameTunnel 客户端安装脚本（Windows PowerShell）
# 用法: 在 PowerShell 中运行
#   irm https://raw.githubusercontent.com/holipay/gametunnel/main/install-client.ps1 | iex
# 或者:
#   .\install-client.ps1 -Server 1.2.3.4

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
Write-Host "  🎮 GameTunnel 客户端安装" -ForegroundColor Cyan
Write-Host ""

# 创建安装目录
if (!(Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
}

# 下载客户端
Write-Host "  📥 下载客户端..." -ForegroundColor Yellow
try {
    Invoke-WebRequest -Uri $ExeUrl -OutFile $ExePath -UseBasicParsing
} catch {
    Write-Host "  ❌ 下载失败: $_" -ForegroundColor Red
    Write-Host "  请手动从 https://github.com/holipay/gametunnel/releases 下载" -ForegroundColor Yellow
    exit 1
}

Write-Host "  ✅ 已下载到 $ExePath" -ForegroundColor Green

# 添加到 PATH（当前用户）
$currentPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($currentPath -notlike "*$InstallDir*") {
    [Environment]::SetEnvironmentVariable("Path", "$currentPath;$InstallDir", "User")
    $env:Path += ";$InstallDir"
    Write-Host "  ✅ 已添加到 PATH" -ForegroundColor Green
}

# 写入配置文件
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
Write-Host "  ✅ 已写入配置 $ConfigDir\config.json" -ForegroundColor Green

# 生成 start.bat 到安装目录
$BatContent = @"
@echo off
chcp 65001 >nul 2>&1
title GameTunnel
net session >nul 2>&1
if %errorlevel% neq 0 (
    echo 请求管理员权限...
    powershell -Command "Start-Process '%~f0' -Verb RunAs"
    exit /b
)
set SERVER=${Server}:4700
set NAME=
set ROOM=${Room}
set PASSWORD=${Password}
set EXE=$ExePath
echo.
echo  ========================================
echo   GameTunnel - 局域网游戏隧道
echo  ========================================
echo   服务器: %SERVER%
echo   房间:   %ROOM%
if defined PASSWORD (echo   认证:   HMAC) else (echo   认证:   无)
echo  ========================================
echo.
set ARGS=-server %SERVER%
if defined NAME set ARGS=%ARGS% -name "%NAME%"
if not "%ROOM%"=="default" set ARGS=%ARGS% -room %ROOM%
if defined PASSWORD set ARGS=%ARGS% -password "%PASSWORD%"
%EXE% %ARGS%
echo.
echo GameTunnel 已退出。
pause
"@
Set-Content -Path "$InstallDir\start.bat" -Value $BatContent -Encoding ASCII
Write-Host "  ✅ 已生成启动器 $InstallDir\start.bat" -ForegroundColor Green

# 创建桌面快捷方式 → start.bat
$WshShell = New-Object -ComObject WScript.Shell
$Shortcut = $WshShell.CreateShortcut("$env:USERPROFILE\Desktop\GameTunnel.lnk")
$Shortcut.TargetPath = "$InstallDir\start.bat"
$Shortcut.WorkingDirectory = $InstallDir
$Shortcut.Description = "GameTunnel - 局域网游戏隧道"
$Shortcut.Save()
Write-Host "  ✅ 已创建桌面快捷方式" -ForegroundColor Green

Write-Host ""
Write-Host "  ✅ 安装完成！" -ForegroundColor Green
Write-Host ""
Write-Host "  启动方式:" -ForegroundColor White
Write-Host "    1. 双击桌面的 GameTunnel 快捷方式" -ForegroundColor White
Write-Host "    2. 运行 $InstallDir\start.bat" -ForegroundColor White
Write-Host "    3. 命令行直接运行:" -ForegroundColor White
Write-Host "       gtunnel-client.exe -server ${Server}:4700" -ForegroundColor Cyan
Write-Host ""
Write-Host "  连接成功后打开游戏，进入局域网模式即可" -ForegroundColor White
Write-Host ""
Write-Host "  ⚠️ 首次运行需要管理员权限（创建虚拟网卡）" -ForegroundColor Yellow
Write-Host ""
