# GameTunnel 客户端安装脚本（Windows PowerShell）
# 用法: 在 PowerShell 中运行
#   irm https://raw.githubusercontent.com/holipay/gametunnel/main/install-client.ps1 | iex
# 或者:
#   .\install-client.ps1 -Server 1.2.3.4

param(
    [Parameter(Mandatory=$true)]
    [string]$Server
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

# 创建桌面快捷方式
$WshShell = New-Object -ComObject WScript.Shell
$Shortcut = $WshShell.CreateShortcut("$env:USERPROFILE\Desktop\GameTunnel.lnk")
$Shortcut.TargetPath = "cmd.exe"
$Shortcut.Arguments = "/k `"$ExePath`" -server ${Server}:4700"
$Shortcut.WorkingDirectory = $InstallDir
$Shortcut.Description = "GameTunnel - 星际争霸1 局域网对战"
$Shortcut.Save()

Write-Host ""
Write-Host "  ✅ 安装完成！" -ForegroundColor Green
Write-Host ""
Write-Host "  连接方式（二选一）:" -ForegroundColor White
Write-Host "    1. 双击桌面的 GameTunnel 快捷方式" -ForegroundColor White
Write-Host "    2. 在 CMD/PowerShell 中运行:" -ForegroundColor White
Write-Host "       gtunnel-client.exe -server ${Server}:4700" -ForegroundColor Cyan
Write-Host ""
Write-Host "  连接成功后打开星际争霸1 → Multiplayer → Local Area Network" -ForegroundColor White
Write-Host ""
Write-Host "  ⚠️ 首次运行需要管理员权限（创建虚拟网卡）" -ForegroundColor Yellow
Write-Host ""
