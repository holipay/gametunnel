#!/bin/bash
# GameTunnel 客户端安装脚本（玩家用）
# Usage: curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/install-client.sh | sudo bash -s -- YOUR_SERVER_IP

set -e

SERVER_IP="${1:-}"
INSTALL_DIR="/usr/local/bin"

if [ -z "$SERVER_IP" ]; then
    echo "用法: sudo bash install-client.sh 服务器IP"
    echo "示例: sudo bash install-client.sh 1.2.3.4"
    exit 1
fi

echo "🎮 GameTunnel 客户端安装"
echo ""

if [ "$EUID" -ne 0 ]; then
    echo "❌ 请用 root 运行: sudo bash install-client.sh $SERVER_IP"
    exit 1
fi

# 检测系统
ARCH=$(uname -m)
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$ARCH" in
    x86_64)  ARCH="amd64" ;;
    aarch64) ARCH="arm64" ;;
esac

# 安装 Go（如果需要编译）
if ! command -v go &>/dev/null; then
    echo "📦 安装 Go..."
    GO_VERSION="1.22.2"
    GO_TAR="go${GO_VERSION}.${OS}-${ARCH}.tar.gz"
    curl -sL "https://go.dev/dl/${GO_TAR}" -o "/tmp/${GO_TAR}"
    tar -C /usr/local -xzf "/tmp/${GO_TAR}"
    rm -f "/tmp/${GO_TAR}"
    export PATH=$PATH:/usr/local/go/bin
fi

# 克隆并编译客户端
echo "🔨 编译客户端..."
TMPDIR=$(mktemp -d)
cd "$TMPDIR"
git clone --depth 1 https://github.com/holipay/gametunnel.git .
make client 2>&1
cp bin/gtunnel-client "$INSTALL_DIR/gtunnel-client"
chmod 755 "$INSTALL_DIR/gtunnel-client"
cd /
rm -rf "$TMPDIR"

echo ""
echo "✅ 安装完成！"
echo ""
echo "  连接命令:"
echo "    sudo gtunnel-client -server ${SERVER_IP}:4700"
echo ""
echo "  连接成功后打开星际争霸1 → Multiplayer → Local Area Network"
echo ""
