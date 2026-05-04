@echo off
chcp 65001 >nul 2>&1
title GameTunnel

:: ──────────────────────────────────────────────
:: GameTunnel 启动器
:: 双击运行即可，会自动请求管理员权限
:: ──────────────────────────────────────────────

:: 检查管理员权限
net session >nul 2>&1
if %errorlevel% neq 0 (
    echo 请求管理员权限...
    powershell -Command "Start-Process '%~f0' -Verb RunAs"
    exit /b
)

:: ── 配置区（修改下面的参数）──────────────────
:: 服务器地址，格式: IP:端口
set SERVER=127.0.0.1:4700

:: 玩家名（留空=使用计算机名）
set NAME=

:: 房间ID
set ROOM=default

:: 房间密码（留空=无密码）
set PASSWORD=
:: ──────────────────────────────────────────────

:: 查找客户端
set EXE=gtunnel-client.exe
where %EXE% >nul 2>&1
if %errorlevel% neq 0 (
    if exist "%~dp0%EXE%" (
        set EXE=%~dp0%EXE%
    ) else (
        echo [错误] 找不到 %EXE%
        echo 请确保 gtunnel-client.exe 在当前目录或 PATH 中
        echo 下载: https://github.com/holipay/gametunnel/releases
        pause
        exit /b 1
    )
)

:: 构建参数
set ARGS=-server %SERVER%
if defined NAME set ARGS=%ARGS% -name "%NAME%"
if not "%ROOM%"=="default" set ARGS=%ARGS% -room %ROOM%
if defined PASSWORD set ARGS=%ARGS% -password "%PASSWORD%"

echo.
echo  ========================================
echo   GameTunnel - 局域网游戏隧道
echo  ========================================
echo   服务器: %SERVER%
if defined NAME echo   玩家:   %NAME%
echo   房间:   %ROOM%
if defined PASSWORD (echo   认证:   HMAC 密码验证) else (echo   认证:   无)
echo  ========================================
echo.

:: 启动
%EXE% %ARGS%

:: 异常退出时暂停，让用户看到错误
echo.
echo GameTunnel 已退出。
pause
