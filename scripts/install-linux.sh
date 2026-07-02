#!/bin/bash
# GameTunnel 一键安装脚本（Linux 服务器）
#
# 用法:
#   在线安装:  curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/scripts/install-linux.sh | sudo bash
#   本地安装:  sudo bash scripts/install-linux.sh              (gtunnel-server-linux-amd64 或 gtunnel-server 在脚本同目录)
#   带密码:    sudo ROOM_PASSWORD=你的密码 bash install-linux.sh
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
ROOM_PASSWORD="${ROOM_PASSWORD:-}"
STATUS_ADDR="${STATUS_ADDR:-}"
STATUS_TOKEN="${STATUS_TOKEN:-}"

# 脚本所在目录（用于本地安装）
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd || pwd)"

# Validate LISTEN_ADDR format and extract port
# Supported formats: ":4700", "0.0.0.0:4700", "[::]:4700", "[2408::1]:4700"
if [[ "$LISTEN_ADDR" == *"[ "* ]] || [[ "$LISTEN_ADDR" == *" ]"* ]]; then
    echo "❌ IPv6 地址方括号内不应有空格: $LISTEN_ADDR"
    exit 1
fi
# Bare IPv6 (no brackets, no port) — user forgot brackets
if [[ "$LISTEN_ADDR" =~ ^[0-9a-fA-F:]+$ ]] && [[ "$LISTEN_ADDR" == *:*:* ]]; then
    echo "❌ IPv6 监听地址需要方括号和端口: [${LISTEN_ADDR}]:4700"
    exit 1
fi
# Extract port: strip everything up to and including the last ':'
LISTEN_PORT="${LISTEN_ADDR##*:}"
if ! [[ "$LISTEN_PORT" =~ ^[0-9]+$ ]]; then
    echo "❌ 无法解析端口号: $LISTEN_ADDR"
    echo "   正确格式: :4700 | 0.0.0.0:4700 | [::]:4700 | [2408::1]:4700"
    exit 1
fi

echo "🎮 GameTunnel Server 安装脚本"
echo ""

# 检查 root
if [ "$EUID" -ne 0 ]; then
    echo "❌ 请用 root 运行此脚本"
    echo "   sudo bash install-linux.sh"
    exit 1
fi

# 检查端口是否被占用（仅新安装时检查，更新跳过）
# 如果是手动运行的 gtunnel-server 占用，视为更新场景，跳过检查
EXISTING_GTUNNEL_PID=$(pgrep -x gtunnel-server 2>/dev/null || true)
if ! systemctl is-active --quiet gtunnel-server 2>/dev/null && [ -z "$EXISTING_GTUNNEL_PID" ]; then
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
USE_LOCAL=false

# 确定架构
ARCH=$(uname -m)
case "$ARCH" in
    x86_64|amd64)   LOCAL_BINARY="gtunnel-server-linux-amd64" ;;
    aarch64|arm64)   LOCAL_BINARY="gtunnel-server-linux-arm64" ;;
    *) echo "❌ 不支持的架构: $ARCH"; exit 1 ;;
esac

# 优先级1: 脚本所在目录有带架构后缀的二进制
if [ -f "$SCRIPT_DIR/$LOCAL_BINARY" ] && file "$SCRIPT_DIR/$LOCAL_BINARY" | grep -q "ELF"; then
    echo "📦 使用本地文件: $SCRIPT_DIR/$LOCAL_BINARY"
    TMPFILE="$SCRIPT_DIR/$LOCAL_BINARY"
    USE_LOCAL=true

# 优先级2: 脚本所在目录有 gtunnel-server 二进制（兼容旧命名）
elif [ -f "$SCRIPT_DIR/$BINARY_NAME" ] && file "$SCRIPT_DIR/$BINARY_NAME" | grep -q "ELF"; then
    echo "📦 使用本地文件: $SCRIPT_DIR/$BINARY_NAME"
    TMPFILE="$SCRIPT_DIR/$BINARY_NAME"
    USE_LOCAL=true

# 优先级3: 当前目录有带架构后缀的二进制
elif [ -f "./$LOCAL_BINARY" ] && file "./$LOCAL_BINARY" | grep -q "ELF"; then
    echo "📦 使用本地文件: ./$LOCAL_BINARY"
    TMPFILE="./$LOCAL_BINARY"
    USE_LOCAL=true

# 优先级4: 当前目录有 gtunnel-server 二进制（兼容旧命名）
elif [ -f "./$BINARY_NAME" ] && file "./$BINARY_NAME" | grep -q "ELF"; then
    echo "📦 使用本地文件: ./$BINARY_NAME"
    TMPFILE="./$BINARY_NAME"
    USE_LOCAL=true
fi

# 优先级5: 从 GitHub 下载
if [ -z "$TMPFILE" ]; then
    # ── 版本检测 ──
    echo "📡 检查最新版本..."

    # 尝试 API（快，但国内偶尔超时）；失败则从 releases 页面提取
    LATEST=""
    API_URL="https://api.github.com/repos/${REPO}/releases/latest"
    RELEASES_URL="https://github.com/${REPO}/releases"

    # 方式1: GitHub API（轻量 JSON）
    LATEST=$(curl -sL --connect-timeout 10 --max-time 15 "$API_URL" 2>/dev/null \
        | grep '"tag_name"' | head -1 | cut -d'"' -f4)

    # 方式2: API 失败时，从 releases 页面 HTML 提取第一个 tag
    if [ -z "$LATEST" ]; then
        echo "  API 超时，尝试从 releases 页面获取..."
        LATEST=$(curl -sL --connect-timeout 10 --max-time 20 "$RELEASES_URL" 2>/dev/null \
            | grep -oP 'href="[^"]*?/releases/tag/[^"]*?"' | head -1 \
            | grep -oP 'tag/[^"]*')
    fi

    if [ -z "$LATEST" ]; then
        echo "❌ 无法获取版本信息，请检查网络或手动下载:"
        echo "   $RELEASES_URL"
        exit 1
    fi
    echo "  最新版本: $LATEST"

    # ── 版本检查：已是最新则跳过 ──
    if [ -f "$INSTALL_DIR/$BINARY_NAME" ]; then
        CURRENT_VERSION=$("$INSTALL_DIR/$BINARY_NAME" -version 2>/dev/null | awk '{print $2}' || echo "")
        if [ "$CURRENT_VERSION" = "$LATEST" ]; then
            echo ""
            echo "✅ 已是最新版本 ($LATEST)，无需更新"
            exit 0
        fi
        if [ -n "$CURRENT_VERSION" ]; then
            echo "  当前版本: $CURRENT_VERSION"
        fi
    fi

    # ── 下载（带重试 + 断点续传）──
    DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${LATEST}/${LOCAL_BINARY}"
    TMPFILE=$(mktemp)
    MAX_RETRY=3

    for attempt in $(seq 1 $MAX_RETRY); do
        echo "📥 下载服务器... (第 ${attempt}/${MAX_RETRY} 次)"
        # -C -: 断点续传  --retry: curl 内置重试  --retry-connrefused: 连接拒绝也重试
        HTTP_CODE=$(curl -L \
            --connect-timeout 15 \
            --max-time 300 \
            --retry 2 \
            --retry-delay 3 \
            --retry-connrefused \
            -C - \
            -w '%{http_code}' \
            -o "$TMPFILE" \
            "$DOWNLOAD_URL" 2>/dev/null)

        if [ "$HTTP_CODE" = "200" ] && file "$TMPFILE" | grep -q "ELF"; then
            break
        fi

        # 4xx 错误不重试（文件不存在等）
        if [[ "$HTTP_CODE" =~ ^4[0-9][0-9]$ ]]; then
            break
        fi

        if [ "$attempt" -lt "$MAX_RETRY" ]; then
            echo "  ⚠️ 下载未成功 (HTTP $HTTP_CODE)，${attempt} 秒后重试..."
            sleep "$attempt"
        fi
    done

    # ── 验证 ──
    if ! file "$TMPFILE" | grep -q "ELF"; then
        echo "❌ 下载失败"
        echo ""
        echo "  可能原因："
        echo "  - GitHub 访问不稳定（国内常见）"
        echo "  - Release 中 ${LOCAL_BINARY} 尚未生成"
        echo ""
        echo "  替代方案（任选其一）："
        echo "  1. 手动下载后放到服务器任意目录，重新运行本脚本："
        echo "     wget ${DOWNLOAD_URL}"
        echo "  2. 在能访问 GitHub 的机器下载，scp 传到服务器"
        echo "  3. 用 gh CLI:  gh release download ${LATEST} -R ${REPO}"
        rm -f "$TMPFILE"
        exit 1
    fi
    echo "  ✅ 下载完成"
fi

# ── 安装 ───────────────────────────────────────────────────────

BACKUP_FILE=""

rollback() {
    if [ -n "$BACKUP_FILE" ] && [ -f "$BACKUP_FILE" ]; then
        echo "🔄 回滚到旧版本..."
        cp "$BACKUP_FILE" "$INSTALL_DIR/$BINARY_NAME"
        systemctl start gtunnel-server 2>/dev/null || true
        echo "  ✅ 已回滚"
    fi
    exit 1
}

# 停止旧服务（更新时：systemd 或手动运行的进程）
if systemctl is-active --quiet gtunnel-server 2>/dev/null; then
    echo "🔄 停止旧服务..."
    systemctl stop gtunnel-server
elif [ -n "$EXISTING_GTUNNEL_PID" ]; then
    echo "🔄 停止手动运行的旧服务 (PID: $EXISTING_GTUNNEL_PID)..."
    kill "$EXISTING_GTUNNEL_PID" 2>/dev/null || true
    sleep 1
fi

# 备份旧版本（更新时）
if [ -f "$INSTALL_DIR/$BINARY_NAME" ]; then
    BACKUP_FILE="/tmp/gtunnel-server.backup.$(date +%s)"
    cp "$INSTALL_DIR/$BINARY_NAME" "$BACKUP_FILE"
    echo "  📦 已备份旧版本到 $BACKUP_FILE"
fi

cp "$TMPFILE" "$INSTALL_DIR/$BINARY_NAME"
chmod 755 "$INSTALL_DIR/$BINARY_NAME"

# 清理临时文件（仅非本地文件）
if [ "$USE_LOCAL" = false ]; then
    rm -f "$TMPFILE"
fi

echo "  ✅ 已安装到 $INSTALL_DIR/$BINARY_NAME"

# ── 创建 systemd 服务 ─────────────────────────────────────────

EXTRA_ARGS=""
if [ -n "$ROOM_PASSWORD" ]; then
    # Validate: systemd ExecStart doesn't support shell quoting, spaces will break it
    if [[ "$ROOM_PASSWORD" == *" "* ]]; then
        echo "❌ 房间密码不能包含空格（systemd ExecStart 不支持 shell 引用）"
        exit 1
    fi
    EXTRA_ARGS="-password ${ROOM_PASSWORD}"
fi
if [ -n "$STATUS_ADDR" ]; then
    # Extract status port for firewall hint
    STATUS_PORT="${STATUS_ADDR##*:}"
    if ! [[ "$STATUS_PORT" =~ ^[0-9]+$ ]]; then
        echo "❌ 无法解析状态页端口号: $STATUS_ADDR"
        echo "   正确格式: :4701 | [::]:4701 | [2408::1]:4701"
        exit 1
    fi
    EXTRA_ARGS="${EXTRA_ARGS} -status-addr ${STATUS_ADDR}"
fi
if [ -n "$STATUS_TOKEN" ]; then
    if [[ "$STATUS_TOKEN" == *" "* ]]; then
        echo "❌ 状态页 token 不能包含空格"
        exit 1
    fi
    EXTRA_ARGS="${EXTRA_ARGS} -status-token ${STATUS_TOKEN}"
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

# ── 健康检查 ───────────────────────────────────────────────────
echo ""
echo "⏳ 等待服务启动..."
sleep 3

# 检查进程是否存活
if ! pgrep -x gtunnel-server >/dev/null 2>&1; then
    echo "❌ 服务启动失败"
    echo "  日志: journalctl -u gtunnel-server -n 20 --no-pager"
    rollback
fi

# 检查端口是否监听
sleep 2
if ss -uln | grep -q ":${LISTEN_PORT} "; then
    echo "✅ 服务已启动，端口 ${LISTEN_PORT} 已监听"
else
    echo "⚠️  进程运行中但端口未监听，可能启动异常"
    echo "  日志: journalctl -u gtunnel-server -n 20 --no-pager"
    rollback
fi

# 清理备份文件
if [ -n "$BACKUP_FILE" ] && [ -f "$BACKUP_FILE" ]; then
    rm -f "$BACKUP_FILE"
fi

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
    echo "  ⚠️ 确保防火墙开放 UDP ${LISTEN_PORT} 和 TCP ${STATUS_PORT} 端口"
    echo "     IPv6: ip6tables -A INPUT -p udp --dport ${LISTEN_PORT} -j ACCEPT"
else
    echo "  ⚠️ 确保防火墙开放 UDP ${LISTEN_PORT} 端口"
    echo "     IPv6: ip6tables -A INPUT -p udp --dport ${LISTEN_PORT} -j ACCEPT"
fi
echo ""
