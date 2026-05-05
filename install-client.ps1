# GameTunnel 客户端安装脚本（Windows PowerShell）
#
# 用法（二选一）:
#   方式1: 下载后运行
#     irm https://raw.githubusercontent.com/holipay/gametunnel/main/install-client.ps1 -OutFile install.ps1
#     .\install.ps1 -Server 111.229.82.204
#
#   方式2: 查看参数后运行
#     .\install-client.ps1 -Server 111.229.82.204 -Password mypass

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

# 检查管理员权限（安装不需要，但首次运行需要）
$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    Write-Host "  ⚠️ 当前非管理员，安装可以继续，但首次运行客户端需要管理员权限" -ForegroundColor Yellow
    Write-Host ""
}

# 创建安装目录
if (!(Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
}

# 下载客户端
Write-Host "  📥 下载客户端..." -ForegroundColor Yellow
try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    Invoke-WebRequest -Uri $ExeUrl -OutFile $ExePath -UseBasicParsing
} catch {
    Write-Host "  ❌ 下载失败: $_" -ForegroundColor Red
    Write-Host "  请手动从 https://github.com/holipay/gametunnel/releases 下载" -ForegroundColor Yellow
    exit 1
}

# 验证下载
if (!(Test-Path $ExePath) -or (Get-Item $ExePath).Length -lt 100000) {
    Write-Host "  ❌ 下载的文件异常（可能被拦截或网络问题）" -ForegroundColor Red
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

# 生成 start.bat（从 config.json 读取配置，不硬编码）
$BatContent = @'
@echo off
:: 切换到 UTF-8 代码页后重新执行自身，确保 cmd.exe 用 UTF-8 读取本文件
if not defined _UTF8_RESTARTED (
    chcp 65001 >nul 2>&1
    set _UTF8_RESTARTED=1
    "%~f0" %*
    exit /b
)
title GameTunnel

:: 请求管理员权限
net session >nul 2>&1
if %errorlevel% neq 0 (
    echo 请求管理员权限...
    powershell -Command "Start-Process '%~f0' -Verb RunAs"
    exit /b
)

set EXE=%LOCALAPPDATA%\GameTunnel\gtunnel-client.exe
if not exist "%EXE%" (
    echo [错误] 找不到 %EXE%
    echo 请重新运行安装脚本
    pause
    exit /b 1
)

:: 从 config.json 读取配置（PowerShell 解析 JSON）
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
echo   GameTunnel - 局域网游戏隧道
echo  ========================================
echo   服务器: %SERVER%
if defined NAME echo   玩家:   %NAME%
echo   房间:   %ROOM%
if defined PASSWORD if not "%PASSWORD%"=="" (echo   认证:   HMAC) else (echo   认证:   无)
echo  ========================================
echo.

set ARGS=-server %SERVER%
if defined NAME if not "%NAME%"=="" set ARGS=%ARGS% -name "%NAME%"
if not "%ROOM%"=="default" set ARGS=%ARGS% -room %ROOM%
if defined PASSWORD if not "%PASSWORD%"=="" set ARGS=%ARGS% -password "%PASSWORD%"

"%EXE%" %ARGS%

echo.
echo GameTunnel 已退出。
pause
'@
Set-Content -Path "$InstallDir\start.bat" -Value $BatContent -Encoding UTF8
Write-Host "  ✅ 已生成启动器 $InstallDir\start.bat" -ForegroundColor Green

# 创建桌面快捷方式
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
Write-Host "    1. 双击桌面 GameTunnel 快捷方式" -ForegroundColor White
Write-Host "    2. 运行 $InstallDir\start.bat" -ForegroundColor White
Write-Host "    3. 命令行:" -ForegroundColor White
Write-Host "       gtunnel-client.exe -server ${Server}:4700" -ForegroundColor Cyan
Write-Host ""
Write-Host "  连接成功后打开游戏，进入局域网模式即可" -ForegroundColor White
Write-Host ""
Write-Host "  ⚠️ 首次运行需要管理员权限（创建 wintun 虚拟网卡）" -ForegroundColor Yellow
Write-Host "  ⚠️ 确保 Windows 防火墙允许 gtunnel-client.exe 访问网络" -ForegroundColor Yellow
Write-Host ""
