#!/bin/bash
# GameTunnel 一键安装脚本（服务器端 - Linux）
# Usage: curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/install.sh | sudo bash
#
# 环境变量:
#   LISTEN_ADDR   - 监听地址 (默认 :4700)
#   SUBNET        - 虚拟子网 (默认 10.10.0.0/24)
#   MAX_PLAYERS   - 最大玩家数 (默认 10)
#   ROOM_PASSWORD - 房间密码 (默认: 无)

set -e

INSTALL_DIR="/usr/local/bin"
SERVICE_FILE="/etc/systemd/system/gtunnel-server.service"
REPO="holipay/gametunnel"
LISTEN_ADDR="${LISTEN_ADDR:-:4700}"
SUBNET="${SUBNET:-10.10.0.0/24}"
MAX_PLAYERS="${MAX_PLAYERS:-10}"

echo "🎮 GameTunnel Server 安装脚本"
echo ""

# 检查 root
if [ "$EUID" -ne 0 ]; then
    echo "❌ 请用 root 运行此脚本"
    echo "   sudo bash install.sh"
    exit 1
fi

# 检查系统架构
ARCH=$(uname -m)
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$ARCH" in
    x86_64)  ARCH="amd64" ;;
    aarch64) ARCH="arm64" ;;
    *)       echo "❌ 不支持的架构: $ARCH"; exit 1 ;;
esac

echo "  系统: $OS/$ARCH"
echo "  监听: $LISTEN_ADDR"
echo "  子网: $SUBNET"
echo "  上限: $MAX_PLAYERS 人"
if [ -n "$ROOM_PASSWORD" ]; then
    echo "  认证: HMAC 密码验证"
else
    echo "  认证: 无"
fi
echo ""

# 获取最新版本号
echo "📡 检查最新版本..."
LATEST=$(curl -sL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | head -1 | cut -d'"' -f4)
if [ -z "$LATEST" ]; then
    echo "⚠️  无法获取版本号，使用 main 分支"
    LATEST="latest"
fi
echo "  版本: $LATEST"

# 下载预编译二进制
BINARY_NAME="gtunnel-server-${OS}-${ARCH}"
if [ "$LATEST" = "latest" ]; then
    DOWNLOAD_URL="https://github.com/${REPO}/releases/latest/download/${BINARY_NAME}"
else
    DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${LATEST}/${BINARY_NAME}"
fi

echo "📥 下载服务器..."
TMPFILE=$(mktemp)
if ! curl -sL "$DOWNLOAD_URL" -o "$TMPFILE"; then
    echo "❌ 下载失败: $DOWNLOAD_URL"
    echo "   请从 https://github.com/${REPO}/releases 手动下载"
    rm -f "$TMPFILE"
    exit 1
fi

# 验证下载的文件是有效的 ELF
if ! file "$TMPFILE" | grep -q "ELF"; then
    echo "❌ 下载的文件不是有效的二进制"
    echo "   可能该架构暂无预编译版本，请手动编译:"
    echo "   git clone https://github.com/${REPO}.git && cd gametunnel && make server"
    rm -f "$TMPFILE"
    exit 1
fi

mv "$TMPFILE" "$INSTALL_DIR/gtunnel-server"
chmod 755 "$INSTALL_DIR/gtunnel-server"
echo "  ✅ 已安装到 $INSTALL_DIR/gtunnel-server"

# 构建启动参数
EXTRA_ARGS=""
if [ -n "$ROOM_PASSWORD" ]; then
    EXTRA_ARGS="-password ${ROOM_PASSWORD}"
fi

# 创建 systemd 服务
cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=GameTunnel Server
After=network.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/gtunnel-server -addr ${LISTEN_ADDR} -subnet ${SUBNET} -max ${MAX_PLAYERS} ${EXTRA_ARGS}
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
echo "  ⚠️ 确保防火墙开放 UDP 4700 端口"
echo ""
