@echo off
chcp 65001 >nul 2>&1
title GameTunnel

:: ──────────────────────────────────────────────
:: GameTunnel 启动器
:: 双击运行即可，会自动请求管理员权限
:: 配置从 %APPDATA%\GameTunnel\config.json 读取
:: （由 install-client.ps1 生成，或手动创建）
:: ──────────────────────────────────────────────

:: 请求管理员权限
net session >nul 2>&1
if %errorlevel% neq 0 (
    echo 请求管理员权限...
    powershell -Command "Start-Process '%~f0' -Verb RunAs"
    exit /b
)

:: 查找客户端
set EXE=gtunnel-client.exe
where %EXE% >nul 2>&1
if %errorlevel% neq 0 (
    if exist "%LOCALAPPDATA%\GameTunnel\gtunnel-client.exe" (
        set EXE=%LOCALAPPDATA%\GameTunnel\gtunnel-client.exe
    ) else if exist "%~dp0gtunnel-client.exe" (
        set EXE=%~dp0gtunnel-client.exe
    ) else (
        echo [错误] 找不到 gtunnel-client.exe
        echo 请确保客户端在当前目录、PATH 或 %LOCALAPPDATA%\GameTunnel 中
        echo 下载: https://github.com/holipay/gametunnel/releases
        pause
        exit /b 1
    )
)

:: 从 config.json 读取配置
set CONFIG_FILE=%APPDATA%\GameTunnel\config.json
if not exist "%CONFIG_FILE%" (
    echo [错误] 找不到配置文件 %CONFIG_FILE%
    echo 请先运行安装脚本，或手动创建配置文件
    echo 格式: {"server_addr":"IP:4700","player_name":"名字","room_id":"default","room_password":""}
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

:: 构建参数
set ARGS=-server %SERVER%
if defined NAME if not "%NAME%"=="" set ARGS=%ARGS% -name "%NAME%"
if not "%ROOM%"=="default" set ARGS=%ARGS% -room %ROOM%
if defined PASSWORD if not "%PASSWORD%"=="" set ARGS=%ARGS% -password "%PASSWORD%"

echo.
echo  ========================================
echo   GameTunnel - 局域网游戏隧道
echo  ========================================
echo   服务器: %SERVER%
if defined NAME if not "%NAME%"=="" echo   玩家:   %NAME%
echo   房间:   %ROOM%
if defined PASSWORD if not "%PASSWORD%"=="" (echo   认证:   HMAC) else (echo   认证:   无)
echo  ========================================
echo.

:: 启动
"%EXE%" %ARGS%

echo.
echo GameTunnel 已退出。
pause
