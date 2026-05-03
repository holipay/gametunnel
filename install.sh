#!/bin/bash
# GameTunnel 一键安装脚本（服务器端）
# Usage: curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/install.sh | bash

set -e

INSTALL_DIR="/usr/local/bin"
SERVICE_FILE="/etc/systemd/system/gtunnel-server.service"
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
echo ""

# 安装 Go（如果需要编译）
if ! command -v go &>/dev/null; then
    echo "📦 安装 Go..."
    GO_VERSION="1.22.2"
    GO_TAR="go${GO_VERSION}.${OS}-${ARCH}.tar.gz"
    curl -sL "https://go.dev/dl/${GO_TAR}" -o "/tmp/${GO_TAR}"
    tar -C /usr/local -xzf "/tmp/${GO_TAR}"
    rm -f "/tmp/${GO_TAR}"
    export PATH=$PATH:/usr/local/go/bin
    echo "  ✅ Go ${GO_VERSION} 已安装"
fi

# 克隆并编译
echo "🔨 编译 GameTunnel..."
TMPDIR=$(mktemp -d)
cd "$TMPDIR"
git clone --depth 1 https://github.com/holipay/gametunnel.git .
make server 2>&1
cp bin/gtunnel-server "$INSTALL_DIR/gtunnel-server"
chmod 755 "$INSTALL_DIR/gtunnel-server"
cd /
rm -rf "$TMPDIR"
echo "  ✅ 已安装到 $INSTALL_DIR/gtunnel-server"

# 创建 systemd 服务
cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=GameTunnel Server
After=network.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/gtunnel-server -addr ${LISTEN_ADDR} -subnet ${SUBNET} -max ${MAX_PLAYERS}
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
echo "  玩家连接命令:"
echo "    sudo gtunnel-client -server 你的IP:4700"
echo ""
echo "  ⚠️ 确保防火墙开放 UDP 4700 端口"
echo ""
