#!/bin/sh
# GameTunnel Server 一键安装脚本（OpenWrt）
#
# 用法:
#   在线安装:  wget -qO- https://raw.githubusercontent.com/holipay/gametunnel/main/scripts/install-openwrt.sh | sh
#   本地安装:  sh install-openwrt.sh                    (二进制文件在脚本同目录)
#   带密码:    ROOM_PASSWORD=你的密码 sh install-openwrt.sh
#
# 环境变量:
#   LISTEN_ADDR   - 监听地址 (默认 :4700)
#   SUBNET        - 虚拟子网 (默认 10.10.0.0/24)
#   MAX_PLAYERS   - 最大玩家数 (默认 10)
#   ROOM_PASSWORD - 房间密码 (默认: 无)
#   STATUS_ADDR   - 状态页面地址 (默认: 禁用，如 :4701)
#   STATUS_TOKEN  - 状态页访问令牌 (默认: 无)

set -e

INSTALL_DIR="/usr/bin"
INIT_SCRIPT="/etc/init.d/gtunnel-server"
CONFIG_FILE="/etc/gtunnel-server.conf"
REPO="holipay/gametunnel"
LISTEN_ADDR="${LISTEN_ADDR:-:4700}"
SUBNET="${SUBNET:-10.10.0.0/24}"
MAX_PLAYERS="${MAX_PLAYERS:-10}"
ROOM_PASSWORD="${ROOM_PASSWORD:-}"
STATUS_ADDR="${STATUS_ADDR:-}"
STATUS_TOKEN="${STATUS_TOKEN:-}"

SCRIPT_DIR="$(cd "$(dirname "$0")" 2>/dev/null && pwd || pwd)"

echo "🎮 GameTunnel Server 安装脚本 (OpenWrt)"
echo ""

# ── 环境检测 ──────────────────────────────────────────────────

# 检查是否 OpenWrt
if [ ! -f /etc/openwrt_version ] && [ ! -f /etc/openwrt_release ] && ! command -v opkg >/dev/null 2>&1; then
    echo "⚠️  未检测到 OpenWrt 环境，继续安装可能不适用"
    echo "   标准 Linux 服务器请使用 install-linux.sh"
    echo ""
fi

# 检查 root
if [ "$(id -u)" -ne 0 ]; then
    echo "❌ 请用 root 运行此脚本"
    echo "   sh install-openwrt.sh"
    exit 1
fi

# 检测架构
ARCH=$(uname -m)
case "$ARCH" in
    aarch64|arm64)
        BINARY_NAME="gtunnel-server-openwrt-arm64"
        ;;
    armv7*|armv7l|arm)
        BINARY_NAME="gtunnel-server-openwrt-armv7"
        ;;
    *)
        echo "❌ 不支持的架构: $ARCH"
        echo "   支持: aarch64/arm64, armv7"
        echo "   低端路由器(MIPS)不推荐使用本工具"
        exit 1
        ;;
esac

echo "  架构: $ARCH → $BINARY_NAME"
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

TMPFILE=""
EXTRACT_DIR=""
USE_LOCAL=false

# 优先级1: 脚本同目录有 gtunnel-server 二进制（已解压）
if [ -f "$SCRIPT_DIR/gtunnel-server" ] && file "$SCRIPT_DIR/gtunnel-server" 2>/dev/null | grep -q "ELF"; then
    echo "📦 使用本地文件: $SCRIPT_DIR/gtunnel-server"
    TMPFILE="$SCRIPT_DIR/gtunnel-server"
    USE_LOCAL=true

# 优先级2: 脚本同目录有架构对应的二进制
elif [ -f "$SCRIPT_DIR/$BINARY_NAME" ] && file "$SCRIPT_DIR/$BINARY_NAME" 2>/dev/null | grep -q "ELF"; then
    echo "📦 使用本地文件: $SCRIPT_DIR/$BINARY_NAME"
    TMPFILE="$SCRIPT_DIR/$BINARY_NAME"
    USE_LOCAL=true

# 优先级3: 脚本同目录有 .tar.gz 压缩包
elif [ -f "$SCRIPT_DIR/GameTunnel-Server-openwrt-arm64.tar.gz" ] && [ "$ARCH" = "aarch64" -o "$ARCH" = "arm64" ]; then
    echo "📦 使用本地压缩包"
    EXTRACT_DIR=$(mktemp -d)
    tar xzf "$SCRIPT_DIR/GameTunnel-Server-openwrt-arm64.tar.gz" -C "$EXTRACT_DIR"
    TMPFILE="$EXTRACT_DIR/gtunnel-server"
    USE_LOCAL=false
elif [ -f "$SCRIPT_DIR/GameTunnel-Server-openwrt-armv7.tar.gz" ] && echo "$ARCH" | grep -q "armv7"; then
    echo "📦 使用本地压缩包"
    EXTRACT_DIR=$(mktemp -d)
    tar xzf "$SCRIPT_DIR/GameTunnel-Server-openwrt-armv7.tar.gz" -C "$EXTRACT_DIR"
    TMPFILE="$EXTRACT_DIR/gtunnel-server"
    USE_LOCAL=false

# 优先级4: 从 GitHub 下载
else
    # ── 版本检测 ──
    echo "📡 检查最新版本..."

    LATEST=""
    API_URL="https://api.github.com/repos/${REPO}/releases/latest"
    RELEASES_URL="https://github.com/${REPO}/releases"

    # 方式1: GitHub API
    LATEST=$(wget -qO- --timeout=15 "$API_URL" 2>/dev/null \
        | grep '"tag_name"' | head -1 | cut -d'"' -f4)

    # 方式2: API 失败时，从 releases 页面 HTML 提取
    if [ -z "$LATEST" ]; then
        echo "  API 超时，尝试从 releases 页面获取..."
        LATEST=$(wget -qO- --timeout=20 "$RELEASES_URL" 2>/dev/null \
            | grep -o '/releases/tag/[^"]*' | head -1 \
            | sed 's|.*/tag/||')
    fi

    if [ -z "$LATEST" ]; then
        echo "❌ 无法获取版本信息，请检查网络或手动下载:"
        echo "   $RELEASES_URL"
        exit 1
    fi
    echo "  版本: $LATEST"

    ARCHIVE_NAME=""
    case "$ARCH" in
        aarch64|arm64) ARCHIVE_NAME="GameTunnel-Server-openwrt-arm64.tar.gz" ;;
        *)             ARCHIVE_NAME="GameTunnel-Server-openwrt-armv7.tar.gz" ;;
    esac

    # ── 下载（带重试）──
    DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${LATEST}/${ARCHIVE_NAME}"
    EXTRACT_DIR=$(mktemp -d)
    MAX_RETRY=3
    DOWNLOADED=false

    for attempt in $(seq 1 $MAX_RETRY); do
        echo "📥 下载... (第 ${attempt}/${MAX_RETRY} 次)"
        # --tries: wget 内置重试  --timeout: 连接/读取超时
        if wget --tries=2 --timeout=30 -qO "$EXTRACT_DIR/pkg.tar.gz" "$DOWNLOAD_URL" 2>/dev/null; then
            if tar xzf "$EXTRACT_DIR/pkg.tar.gz" -C "$EXTRACT_DIR" 2>/dev/null; then
                DOWNLOADED=true
                break
            fi
        fi

        if [ "$attempt" -lt "$MAX_RETRY" ]; then
            echo "  ⚠️ 下载未成功，${attempt} 秒后重试..."
            sleep "$attempt"
        fi
    done

    rm -f "$EXTRACT_DIR/pkg.tar.gz"

    if [ "$DOWNLOADED" = false ]; then
        echo "❌ 下载失败"
        echo ""
        echo "  替代方案（任选其一）："
        echo "  1. 在能访问 GitHub 的机器下载后 scp 传到路由器"
        echo "  2. 手动下载: wget ${DOWNLOAD_URL}"
        rm -rf "$EXTRACT_DIR"
        exit 1
    fi

    TMPFILE="$EXTRACT_DIR/gtunnel-server"
    if [ ! -f "$TMPFILE" ] || ! file "$TMPFILE" 2>/dev/null | grep -q "ELF"; then
        echo "❌ 无效的二进制文件"
        rm -rf "$EXTRACT_DIR"
        exit 1
    fi
    USE_LOCAL=false
    echo "  ✅ 下载完成"
fi

# ── 安装 ───────────────────────────────────────────────────────

# 停止旧服务
if [ -x "$INIT_SCRIPT" ]; then
    echo "🔄 停止旧服务..."
    "$INIT_SCRIPT" stop 2>/dev/null || true
fi

cp "$TMPFILE" "$INSTALL_DIR/gtunnel-server"
chmod 755 "$INSTALL_DIR/gtunnel-server"

if [ "$USE_LOCAL" = false ]; then
    rm -f "$TMPFILE"
    [ -n "$EXTRACT_DIR" ] && rm -rf "$EXTRACT_DIR"
fi

echo "  ✅ 已安装到 $INSTALL_DIR/gtunnel-server"

# ── 生成配置文件 ───────────────────────────────────────────────

cat > "$CONFIG_FILE" <<EOF
# GameTunnel Server 配置 (OpenWrt)
LISTEN_ADDR="${LISTEN_ADDR}"
SUBNET="${SUBNET}"
MAX_PLAYERS="${MAX_PLAYERS}"
ROOM_PASSWORD="${ROOM_PASSWORD}"
STATUS_ADDR="${STATUS_ADDR}"
STATUS_TOKEN="${STATUS_TOKEN}"
EOF
chmod 0600 "$CONFIG_FILE"
echo "  ✅ 配置已写入 $CONFIG_FILE"

# ── 创建 procd init 脚本 ──────────────────────────────────────

cat > "$INIT_SCRIPT" <<'INITEOF'
#!/bin/sh /etc/rc.common
# GameTunnel Server init script for OpenWrt (procd)

START=99
STOP=10
USE_PROCD=1

PROG=/usr/bin/gtunnel-server
CONFIG=/etc/gtunnel-server.conf

start_service() {
    # 读取配置
    local listen_addr=":4700"
    local subnet="10.10.0.0/24"
    local max_players="10"
    local room_password=""
    local status_addr=""
    local status_token=""

    [ -f "$CONFIG" ] && . "$CONFIG"

    [ -n "$LISTEN_ADDR" ] && listen_addr="$LISTEN_ADDR"
    [ -n "$SUBNET" ] && subnet="$SUBNET"
    [ -n "$MAX_PLAYERS" ] && max_players="$MAX_PLAYERS"

    procd_open_instance gtunnel-server
    procd_set_param command "$PROG" \
        -addr "$listen_addr" \
        -subnet "$subnet" \
        -max "$max_players"
    [ -n "$ROOM_PASSWORD" ] && procd_append_param command -password "$ROOM_PASSWORD"
    [ -n "$STATUS_ADDR" ] && procd_append_param command -status-addr "$STATUS_ADDR"
    [ -n "$STATUS_TOKEN" ] && procd_append_param command -status-token "$STATUS_TOKEN"
    procd_set_param respawn 3600 5 5
    procd_set_param stdout 1
    procd_set_param stderr 1
    procd_close_instance
}

reload_service() {
    stop
    start
}
INITEOF

chmod +x "$INIT_SCRIPT"
echo "  ✅ init 脚本已创建: $INIT_SCRIPT"

# ── 防火墙规则 ─────────────────────────────────────────────────

LISTEN_PORT="${LISTEN_ADDR##*:}"

# 检查是否已有规则
if ! uci show firewall 2>/dev/null | grep -q "GameTunnel"; then
    echo "🔥 配置防火墙规则..."
    RULE_INDEX=$(uci add firewall rule)
    uci set firewall.${RULE_INDEX}.name='GameTunnel'
    uci set firewall.${RULE_INDEX}.src='wan'
    uci set firewall.${RULE_INDEX}.proto='udp'
    uci set firewall.${RULE_INDEX}.dest_port="${LISTEN_PORT}"
    uci set firewall.${RULE_INDEX}.target='ACCEPT'
    uci commit firewall

    if [ -n "$STATUS_ADDR" ]; then
        STATUS_PORT="${STATUS_ADDR##*:}"
        RULE_INDEX=$(uci add firewall rule)
        uci set firewall.${RULE_INDEX}.name='GameTunnel-Status'
        uci set firewall.${RULE_INDEX}.src='wan'
        uci set firewall.${RULE_INDEX}.proto='tcp'
        uci set firewall.${RULE_INDEX}.dest_port="${STATUS_PORT}"
        uci set firewall.${RULE_INDEX}.target='ACCEPT'
        uci commit firewall
    fi

    /etc/init.d/firewall reload 2>/dev/null || true
    echo "  ✅ 防火墙已放行 UDP ${LISTEN_PORT}"
else
    echo "  ℹ️  防火墙规则已存在，跳过"
fi

# ── 内存优化（可选） ───────────────────────────────────────────

# 检测可用内存，给出建议
MEM_TOTAL=$(grep MemTotal /proc/meminfo 2>/dev/null | awk '{print $2}')
if [ -n "$MEM_TOTAL" ] && [ "$MEM_TOTAL" -lt 262144 ]; then
    echo ""
    echo "  ⚠️  检测到内存 < 256MB，建议设置较低的最大玩家数 (-max 5)"
fi

# ── 启动服务 ───────────────────────────────────────────────────

echo ""
echo "🚀 启动服务..."
"$INIT_SCRIPT" enable 2>/dev/null || true
"$INIT_SCRIPT" start 2>/dev/null || true

# 等待启动
sleep 1
if pgrep -x gtunnel-server >/dev/null 2>&1; then
    echo "  ✅ 服务已启动"
else
    echo "  ⚠️  服务可能未成功启动，请检查日志:"
    echo "     logread | grep gtunnel"
fi

echo ""
echo "═══════════════════════════════════════════════════════════"
echo "  ✅ 安装完成！"
echo "═══════════════════════════════════════════════════════════"
echo ""
echo "  管理命令:"
echo "    /etc/init.d/gtunnel-server start    # 启动"
echo "    /etc/init.d/gtunnel-server stop     # 停止"
echo "    /etc/init.d/gtunnel-server restart  # 重启"
echo "    /etc/init.d/gtunnel-server status   # 状态"
echo ""
echo "  日志查看:"
echo "    logread | grep gtunnel              # 查看日志"
echo "    logread -f | grep gtunnel           # 实时日志"
echo ""
echo "  配置文件: $CONFIG_FILE"
echo "  编辑后重启服务生效: /etc/init.d/gtunnel-server restart"
echo ""
echo "  玩家下载客户端: https://github.com/${REPO}/releases"
echo ""
echo "  ⚠️  确保防火墙已放行 UDP ${LISTEN_PORT} 端口"
echo "     本脚本已自动配置，如手动管理请检查: uci show firewall | grep GameTunnel"
echo ""
