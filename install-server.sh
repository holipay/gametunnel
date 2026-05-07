#!/bin/bash
# GameTunnel 一键安装脚本（服务器端 - Linux）
#
# 用法:
#   在线安装:  curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/install-server.sh | sudo bash
#   本地安装:  sudo bash install-server.sh                    (gtunnel-server 在脚本同目录)
#   带密码:    sudo ROOM_PASSWORD=你的密码 bash install-server.sh
#
# 环境变量:
#   LISTEN_ADDR   - 监听地址 (默认 :4700)
#   SUBNET        - 虚拟子网 (默认 10.10.0.0/24)
#   MAX_PLAYERS   - 最大玩家数 (默认 10)
#   ROOM_PASSWORD - 房间密码 (默认: 无)
#   STATUS_ADDR   - 状态页面地址 (默认: 禁用，如 :4701)

set -e

INSTALL_DIR="/usr/local/bin"
SERVICE_FILE="/etc/systemd/system/gtunnel-server.service"
REPO="holipay/gametunnel"
LISTEN_ADDR="${LISTEN_ADDR:-:4700}"
SUBNET="${SUBNET:-10.10.0.0/24}"
MAX_PLAYERS="${MAX_PLAYERS:-10}"
STATUS_ADDR="${STATUS_ADDR:-}"

# 脚本所在目录（用于本地安装）
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd || pwd)"

# Extract port from LISTEN_ADDR (e.g., ":4700" → "4700")
LISTEN_PORT="${LISTEN_ADDR##*:}"

echo "🎮 GameTunnel Server 安装脚本"
echo ""

# 检查 root
if [ "$EUID" -ne 0 ]; then
    echo "❌ 请用 root 运行此脚本"
    echo "   sudo bash install-server.sh"
    exit 1
fi

# 检查端口是否被占用（仅新安装时检查，更新跳过）
if ! systemctl is-active --quiet gtunnel-server 2>/dev/null; then
    if ss -uln | grep -q ":${LISTEN_PORT} "; then
        echo "❌ UDP 端口 ${LISTEN_PORT} 已被占用"
        echo "   请先停止占用该端口的服务，或用 LISTEN_ADDR=:其他端口 指定新端口"
        exit 1
    fi
fi

echo "  监听: $LISTEN_ADDR"
echo "  子网: $SUBNET"
echo "  上限: $MAX_PLAYERS 人"
if [ -n "$ROOM_PASSWORD" ]; then
    echo "  认证: HMAC 密码验证"
else
    echo "  认证: 无"
fi
if [ -n "$STATUS_ADDR" ]; then
    echo "  状态: http://${STATUS_ADDR}"
fi
echo ""

# ── 获取二进制文件 ─────────────────────────────────────────────

BINARY_NAME="gtunnel-server"
TMPFILE=""
EXTRACT_DIR=""
USE_LOCAL=false

# 优先级1: 脚本所在目录有 gtunnel-server
if [ -f "$SCRIPT_DIR/$BINARY_NAME" ] && file "$SCRIPT_DIR/$BINARY_NAME" | grep -q "ELF"; then
    echo "📦 使用本地文件: $SCRIPT_DIR/$BINARY_NAME"
    TMPFILE="$SCRIPT_DIR/$BINARY_NAME"
    USE_LOCAL=true

# 优先级2: 当前目录有 gtunnel-server
elif [ -f "./$BINARY_NAME" ] && file "./$BINARY_NAME" | grep -q "ELF"; then
    echo "📦 使用本地文件: ./$BINARY_NAME"
    TMPFILE="./$BINARY_NAME"
    USE_LOCAL=true
fi

# 优先级3: 从 GitHub 下载
if [ -z "$TMPFILE" ]; then
    echo "📡 检查最新版本..."
    LATEST=$(curl -sL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | head -1 | cut -d'"' -f4)
    if [ -z "$LATEST" ]; then
        echo "❌ 无法获取版本信息，请检查网络或手动下载:"
        echo "   https://github.com/${REPO}/releases"
        exit 1
    fi
    echo "  版本: $LATEST"

    # 根据系统架构选择下载包
    ARCH=$(uname -m)
    case "$ARCH" in
        x86_64|amd64)   ARCHIVE_NAME="GameTunnel-linux-amd64.tar.gz" ;;
        aarch64|arm64)   ARCHIVE_NAME="GameTunnel-linux-arm64.tar.gz" ;;
        *) echo "❌ 不支持的架构: $ARCH"; exit 1 ;;
    esac
    DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${LATEST}/${ARCHIVE_NAME}"
    echo "📥 下载服务器..."
    TMPFILE=$(mktemp)
    if ! curl -sL "$DOWNLOAD_URL" -o "$TMPFILE"; then
        echo "❌ 下载失败: $DOWNLOAD_URL"
        echo ""
        echo "  替代方案："
        echo "  1. 从 https://github.com/${REPO}/releases 手动下载 ${ARCHIVE_NAME}"
        echo "  2. 解压后放到服务器上，和 install-server.sh 同目录，重新运行"
        rm -f "$TMPFILE"
        exit 1
    fi

    # 解压提取 gtunnel-server
    EXTRACT_DIR=$(mktemp -d)
    if ! tar xzf "$TMPFILE" -C "$EXTRACT_DIR" $BINARY_NAME 2>/dev/null; then
        echo "❌ 解压失败: $ARCHIVE_NAME"
        echo ""
        echo "  替代方案："
        echo "  1. 从 https://github.com/${REPO}/releases 手动下载 ${ARCHIVE_NAME}"
        echo "  2. 解压后放到服务器上，和 install-server.sh 同目录，重新运行"
        rm -f "$TMPFILE"
        rm -rf "$EXTRACT_DIR"
        exit 1
    fi
    rm -f "$TMPFILE"
    TMPFILE="$EXTRACT_DIR/$BINARY_NAME"

    # 验证是 ELF 二进制
    if ! file "$TMPFILE" | grep -q "ELF"; then
        echo "❌ 解压后的文件不是有效的 Linux 二进制"
        echo ""
        echo "  替代方案："
        echo "  1. 从 https://github.com/${REPO}/releases 手动下载 ${ARCHIVE_NAME}"
        echo "  2. 解压后放到服务器上，和 install-server.sh 同目录，重新运行"
        rm -rf "$EXTRACT_DIR"
        exit 1
    fi
    echo "  ✅ 下载完成"
fi

# ── 安装 ───────────────────────────────────────────────────────

# 停止旧服务（更新时）
if systemctl is-active --quiet gtunnel-server 2>/dev/null; then
    echo "🔄 停止旧服务..."
    systemctl stop gtunnel-server
fi

cp "$TMPFILE" "$INSTALL_DIR/$BINARY_NAME"
chmod 755 "$INSTALL_DIR/$BINARY_NAME"

# 清理临时文件（仅非本地文件）
if [ "$USE_LOCAL" = false ]; then
    rm -f "$TMPFILE"
    [ -n "$EXTRACT_DIR" ] && rm -rf "$EXTRACT_DIR"
fi

echo "  ✅ 已安装到 $INSTALL_DIR/$BINARY_NAME"

# ── 创建 systemd 服务 ─────────────────────────────────────────

EXTRA_ARGS=""
if [ -n "$ROOM_PASSWORD" ]; then
    EXTRA_ARGS="-password ${ROOM_PASSWORD}"
fi
if [ -n "$STATUS_ADDR" ]; then
    EXTRA_ARGS="${EXTRA_ARGS} -status-addr ${STATUS_ADDR}"
fi

cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=GameTunnel Server
After=network.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/$BINARY_NAME -addr ${LISTEN_ADDR} -subnet ${SUBNET} -max ${MAX_PLAYERS} ${EXTRA_ARGS}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable gtunnel-server
systemctl start gtunnel-server

echo ""
echo "✅ 安装完成！"
echo ""
echo "  状态: systemctl status gtunnel-server"
echo "  日志: journalctl -u gtunnel-server -f"
echo "  停止: systemctl stop gtunnel-server"
echo ""
echo "  玩家下载客户端: https://github.com/${REPO}/releases"
echo ""
if [ -n "$STATUS_ADDR" ]; then
    STATUS_PORT="${STATUS_ADDR##*:}"
    echo "  ⚠️ 确保防火墙开放 UDP ${LISTEN_PORT} 和 TCP ${STATUS_PORT} 端口"
else
    echo "  ⚠️ 确保防火墙开放 UDP ${LISTEN_PORT} 端口"
fi
echo ""
