// Package i18n provides simple internationalization for GameTunnel.
package i18n

import "fmt"

// Lang represents a supported language.
type Lang string

const (
	ZH Lang = "zh"
	EN Lang = "en"
)

// Strings holds all translatable strings.
type Strings struct {
	// ── Server ──
	ServerBanner      string // "GameTunnel Server 局域网游戏隧道"
	ServerAddr        string // "地址"
	ServerSubnet      string // "子网"
	ServerMaxPlayers  string // "最大玩家"
	ServerAuth        string // "认证"
	ServerVersion     string // "版本"
	ServerStatusPage  string // "状态页"
	ServerNoAuth      string // "无认证"
	ServerHMACAuth    string // "HMAC 认证 (基于房间ID)"
	ServerSignal      string // "收到信号 %v，正在关闭..."
	ServerShutdown    string // "服务器已关闭"
	ServerStartFail   string // "启动失败: %v"
	ServerInstRunning string // "检测到已有实例运行中: %v\n请先关闭已有的 GameTunnel 服务端再重试。"
	ServerSubnetFail  string // "子网解析失败 %s: %v"
	ServerSubnetMust  string // "子网必须是 /24 (当前: /%d)"
	ServerSendFail    string // "[server] 发送失败 (累计%d次): %v"
	ServerStatusLog   string // "[status] 状态页面: http://%s"
	ServerStatusFail  string // "[status] HTTP 服务启动失败: %v"
	ServerTmplFail    string // "[status] 模板渲染失败: %v"
	ServerPeerLeave   string // "[-] %s (%s) 超时断开"

	// ── Server Status Page HTML ──
	StatusTitle       string // "GameTunnel Server"
	StatusPlayers     string // "玩家"
	StatusUptime      string // "运行时间"
	StatusVersion     string // "版本"
	StatusNoPlayers   string // "暂无玩家连接"
	StatusTablePlayer string // "玩家"
	StatusTableVIP    string // "虚拟 IP"
	StatusTableAddr   string // "地址"
	StatusTablePing   string // "延迟"
	StatusTableIdle   string // "空闲"
	StatusAuthHMAC    string // "HMAC 认证"
	StatusAuthNone    string // "无认证"
	StatusJustNow     string // "刚刚"
	StatusSecAgo      string // "%ds前"

	// ── Client Tray ──
	TrayTitle         string // "GameTunnel"
	TrayTooltip       string // "GameTunnel - 未连接"
	TrayStatusOffline string // "🔴 未连接"
	TrayConnect       string // "⚡ 连接"
	TrayConnectDesc   string // "连接到服务器"
	TrayDisconnect    string // "🔌 断开"
	TrayDisconnectDesc string // "断开当前连接"
	TraySettings      string // "⚙ 设置..."
	TraySettingsDesc  string // "配置服务器和玩家信息"
	TrayViewLog       string // "📄 查看日志"
	TrayOpenLogFile   string // "打开日志文件"
	TrayQuit          string // "❌ 退出"
	TrayQuitDesc      string // "退出 GameTunnel"
	TrayConnecting    string // "🟡 连接中..."
	TrayTooltipConn   string // "GameTunnel - 连接中..."
	TrayStatusOnline  string // "🟢 %s · %d人"
	TrayTooltipOnline string // "GameTunnel - %s · %d人在线"
	TrayCfgUpdated    string // "[tray] 配置已更新"
	TrayNoServer      string // "请先配置服务器地址"
	TrayStatusError   string // "🔴 连接失败"
	TrayEditConfig    string // "📝 编辑配置文件"
	TrayEditConfigDesc string // "用记事本打开配置文件"

	// ── Client Dialog ──
	DlgTitle       string // "GameTunnel 设置"
	DlgServerAddr  string // "服务器地址:"
	DlgPlayerName  string // "玩家名称:"
	DlgRoomID      string // "房间 ID:"
	DlgPassword    string // "密码:"
	DlgOK          string // "确定"
	DlgCancel      string // "取消"
	DlgStatusIdle  string // "未连接"
	DlgStatusConn  string // "已连接 · %s · %d人在线"
	DlgFirstRun    string // "首次设置 - GameTunnel"
	DlgConnect     string // "连接"
	DlgShowPass    string // "显示密码"
	DlgInvalidAddr string // "服务器地址格式不正确，应为 IP:端口"

	// ── Client App ──
	AppAutoConnect string // "[app] 自动连接到 %s"
	AppSaveFail    string // "[app] 保存配置失败: %v"
	AppDisconnected string // "[app] 连接断开"
	AppDisconnectErr string // "[app] 连接断开: %v"

	// ── Connection Error Dialog ──
	ConnErrTitle     string // "连接失败"
	ConnErrRetry     string // "重试"
	ConnErrSettings  string // "修改设置"
	ConnErrStop      string // "停止连接"
	ConnErrBalloon   string // "GameTunnel 已启动，点击右下角托盘图标进行设置"
	ConnErrBalloonTitle string // "GameTunnel"
	FirstRunBalloon  string // "GameTunnel 已启动！请点击右下角托盘图标进行首次设置"
	DlgNameEmpty     string // "玩家名称不能为空"

	// ── Client Run ──
	RunStartup string // "=== GameTunnel 启动 ==="

	// ── Config ──
	CfgHeader     string // "# GameTunnel Configuration"
	CfgServerHint string // "# Server address (required, e.g. 1.2.3.4:4700)"
	CfgNameHint   string // "# Player name (default: computer name)"
	CfgRoomHint   string // "# Room ID (default: default)"
	CfgPassHint   string // "# Password (leave empty if none)"

	// ── Server Register / Kick ──
	KickInvalidName    string // "用户名不合法"
	KickInvalidRoom    string // "房间ID不合法"
	KickRateLimit      string // "注册过于频繁，请稍后再试"
	KickIPLimit        string // "同一IP连接数已达上限"
	KickAuthPending    string // "认证进行中，请等待"
	KickRoomFull       string // "房间已满"
	KickDuplicateName  string // "同房间内已存在相同用户名的玩家，请更换用户名"
	KickServerBusy     string // "服务器繁忙，请稍后重试"
	KickIPExhausted    string // "IP已分配完"
	KickInternalError  string // "服务器内部错误"
	KickAuthAbnormal   string // "认证状态异常"
	KickAuthTimeout    string // "认证超时"
	KickWrongPassword  string // "密码错误"
	LogPlayerJoin      string // "[+] %s (%s) → %s  [在线: %d]"
	LogChallengeFail   string // "[auth] 生成 challenge 失败: %v"
	LogAuthFail        string // "[auth] 认证失败: %s (%s)"
	LogAuthPass        string // "[auth] 认证通过: %s (%s)"
	LogPlayerLeave     string // "[-] %s (%s) 主动断开"

	// ── Client Tunnel ──
	ErrInvalidServer   string // "服务器地址无效: %w"
	ErrBindUDP         string // "绑定 UDP 失败: %w"
	ErrRegisterFailed  string // "注册失败: %w"
	ErrTooManyAuth     string // "认证失败：服务器发送了过多的认证请求"
	ErrRejected        string // "被拒绝: %s"
	ErrNeedPassword    string // "服务器需要房间密码，请用 -password 参数指定"
	ErrCreateTUN       string // "创建 TUN 失败: %w"
	ErrDecodeFailed    string // "解码响应失败: %w"
	ErrParseIPFailed   string // "解析IP分配失败: %w"
	ErrParseAuthFailed string // "解析认证请求失败: %w"
	ErrDeriveKeyFailed string // "无法派生认证密钥"
	ErrElevateFailed   string // "  提权失败: %v\n"
	LogReuseTUN        string // "[tunnel] 复用 TUN 设备 (IP %s 未变)"
	LogIPChanged       string // "[tunnel] IP 变更 %s → %s，重建 TUN 设备"
	LogPeerExit        string // "[tunnel] %s 退出，断开连接"
	LogTunnelDisconnect string // "[tunnel] 断开连接"
	LogSendFail        string // "[tunnel] 发送失败 (累计%d次): %v"
	LogP2PSuccess      string // "[tunnel] P2P 打洞成功 (phase %d): %s"
	LogP2PFailed       string // "[tunnel] P2P 打洞完成（未确认直通），将通过中继通信: %s"
	LogServerTimeout   string // "[tunnel] 服务器无响应超过 %v，连接可能已断开"
	LogCleanPeer       string // "[tunnel] 清理过期玩家: %s (%s) — 超过 %v 未响应"
	LogRetryPunch      string // "[tunnel] 重试打洞: %d 个未直通的玩家"
	LogRegTimeout      string // "[tunnel] 注册超时，重试 %d/%d..."
	LogRegFailed       string // "注册失败（重试%d次）"
	LogAuthSent        string // "[tunnel] 已发送认证响应，等待服务器确认..."
	LogReadConsecFail  string // "[tunnel] 服务端连接读取连续失败 %d 次，退出: %v"
	LogTUNWriteFail    string // "[tunnel] TUN 写入失败: %v"
	LogPeerAddrChange  string // "[tunnel] 玩家地址变更: %s (%s) %s → %s"
	LogNewPeer         string // "[tunnel] 新玩家: %s (%s)"
	LogPeerLeave2      string // "[tunnel] 玩家离开: %s (%s)"
	LogTUNConsecFail   string // "[tunnel] TUN 设备读取连续失败 %d 次，退出: %v"
}

// zhStrings is the Chinese language pack.
var zhStrings = &Strings{
	ServerBanner:      "▎         GameTunnel Server 局域网游戏隧道                ▎",
	ServerAddr:        "地址",
	ServerSubnet:      "子网",
	ServerMaxPlayers:  "最大玩家",
	ServerAuth:        "认证",
	ServerVersion:     "版本",
	ServerStatusPage:  "状态页",
	ServerNoAuth:      "无认证",
	ServerHMACAuth:    "HMAC 认证 (基于房间ID)",
	ServerSignal:      "收到信号 %v，正在关闭...",
	ServerShutdown:    "服务器已关闭",
	ServerStartFail:   "启动失败: %v",
	ServerInstRunning: "检测到已有实例运行中: %v\n请先关闭已有的 GameTunnel 服务端再重试。",
	ServerSubnetFail:  "子网解析失败 %s: %v",
	ServerSubnetMust:  "子网必须是 /24 (当前: /%d)",
	ServerSendFail:    "[server] 发送失败 (累计%d次): %v",
	ServerStatusLog:   "[status] 状态页面: http://%s",
	ServerStatusFail:  "[status] HTTP 服务启动失败: %v",
	ServerTmplFail:    "[status] 模板渲染失败: %v",
	ServerPeerLeave:   "[-] %s (%s) 超时断开",

	StatusTitle:       "GameTunnel Server",
	StatusPlayers:     "玩家",
	StatusUptime:      "运行时间",
	StatusVersion:     "版本",
	StatusNoPlayers:   "暂无玩家连接",
	StatusTablePlayer: "玩家",
	StatusTableVIP:    "虚拟 IP",
	StatusTableAddr:   "地址",
	StatusTablePing:   "延迟",
	StatusTableIdle:   "空闲",
	StatusAuthHMAC:    "HMAC 认证",
	StatusAuthNone:    "无认证",
	StatusJustNow:     "刚刚",
	StatusSecAgo:      "%ds前",

	TrayTitle:          "GameTunnel",
	TrayTooltip:        "GameTunnel - 未连接",
	TrayStatusOffline:  "🔴 未连接",
	TrayConnect:        "⚡ 连接",
	TrayConnectDesc:    "连接到服务器",
	TrayDisconnect:     "🔌 断开",
	TrayDisconnectDesc: "断开当前连接",
	TraySettings:       "⚙ 设置...",
	TraySettingsDesc:   "配置服务器和玩家信息",
	TrayViewLog:        "📄 查看日志",
	TrayOpenLogFile:    "打开日志文件",
	TrayQuit:           "❌ 退出",
	TrayQuitDesc:       "退出 GameTunnel",
	TrayConnecting:     "🟡 连接中...",
	TrayTooltipConn:    "GameTunnel - 连接中...",
	TrayStatusOnline:   "🟢 %s · %d人",
	TrayTooltipOnline:  "GameTunnel - %s · %d人在线",
	TrayCfgUpdated:     "[tray] 配置已更新",
	TrayNoServer:       "请先配置服务器地址",
	TrayStatusError:    "🔴 连接失败",
	TrayEditConfig:     "📝 编辑配置文件",
	TrayEditConfigDesc: "用记事本打开配置文件",

	DlgTitle:      "GameTunnel 设置",
	DlgServerAddr: "服务器地址:",
	DlgPlayerName: "玩家名称:",
	DlgRoomID:     "房间 ID:",
	DlgPassword:   "密码:",
	DlgOK:         "确定",
	DlgCancel:     "取消",
	DlgStatusIdle: "未连接",
	DlgStatusConn: "已连接 · %s · %d人在线",
	DlgFirstRun:   "首次设置 - GameTunnel",
	DlgConnect:    "连接",
	DlgShowPass:   "显示密码",
	DlgInvalidAddr: "服务器地址格式不正确，应为 IP:端口",

	AppAutoConnect:   "[app] 自动连接到 %s",
	AppSaveFail:      "[app] 保存配置失败: %v",
	AppDisconnected:  "[app] 连接断开",
	AppDisconnectErr: "[app] 连接断开: %v",

	ConnErrTitle:        "连接失败",
	ConnErrRetry:        "重试",
	ConnErrSettings:     "修改设置",
	ConnErrStop:         "停止连接",
	ConnErrBalloon:      "GameTunnel 已启动，点击右下角托盘图标进行设置",
	ConnErrBalloonTitle: "GameTunnel",
	FirstRunBalloon:     "GameTunnel 已启动！请点击右下角托盘图标进行首次设置",
	DlgNameEmpty:        "玩家名称不能为空",

	RunStartup: "=== GameTunnel 启动 ===",

	CfgHeader:     "# GameTunnel 配置文件",
	CfgServerHint: "# 服务器地址（必填，如 1.2.3.4:4700 或 [2408::1]:4700）",
	CfgNameHint:   "# 玩家名称（默认：计算机名）",
	CfgRoomHint:   "# 房间 ID（默认：default）",
	CfgPassHint:   "# 密码（无密码留空）",

	KickInvalidName:   "用户名不合法",
	KickInvalidRoom:   "房间ID不合法",
	KickRateLimit:     "注册过于频繁，请稍后再试",
	KickIPLimit:       "同一IP连接数已达上限",
	KickAuthPending:   "认证进行中，请等待",
	KickRoomFull:      "房间已满",
	KickDuplicateName: "同房间内已存在相同用户名的玩家，请更换用户名",
	KickServerBusy:    "服务器繁忙，请稍后重试",
	KickIPExhausted:   "IP已分配完",
	KickInternalError: "服务器内部错误",
	KickAuthAbnormal:  "认证状态异常",
	KickAuthTimeout:   "认证超时",
	KickWrongPassword: "密码错误",
	LogPlayerJoin:     "[+] %s (%s) → %s  [在线: %d]",
	LogChallengeFail:  "[auth] 生成 challenge 失败: %v",
	LogAuthFail:       "[auth] 认证失败: %s (%s)",
	LogAuthPass:       "[auth] 认证通过: %s (%s)",
	LogPlayerLeave:    "[-] %s (%s) 主动断开",

	ErrInvalidServer:    "服务器地址无效: %w",
	ErrBindUDP:          "绑定 UDP 失败: %w",
	ErrRegisterFailed:   "注册失败: %w",
	ErrTooManyAuth:      "认证失败：服务器发送了过多的认证请求",
	ErrRejected:         "被拒绝: %s",
	ErrNeedPassword:     "服务器需要房间密码，请用 -password 参数指定",
	ErrCreateTUN:        "创建 TUN 失败: %w",
	ErrDecodeFailed:     "解码响应失败: %w",
	ErrParseIPFailed:    "解析IP分配失败: %w",
	ErrParseAuthFailed:  "解析认证请求失败: %w",
	ErrDeriveKeyFailed:  "无法派生认证密钥",
	ErrElevateFailed:    "  提权失败: %v\n",
	LogReuseTUN:         "[tunnel] 复用 TUN 设备 (IP %s 未变)",
	LogIPChanged:        "[tunnel] IP 变更 %s → %s，重建 TUN 设备",
	LogPeerExit:         "[tunnel] %s 退出，断开连接",
	LogTunnelDisconnect: "[tunnel] 断开连接",
	LogSendFail:         "[tunnel] 发送失败 (累计%d次): %v",
	LogP2PSuccess:       "[tunnel] P2P 打洞成功 (phase %d): %s",
	LogP2PFailed:        "[tunnel] P2P 打洞完成（未确认直通），将通过中继通信: %s",
	LogServerTimeout:    "[tunnel] 服务器无响应超过 %v，连接可能已断开",
	LogCleanPeer:        "[tunnel] 清理过期玩家: %s (%s) — 超过 %v 未响应",
	LogRetryPunch:       "[tunnel] 重试打洞: %d 个未直通的玩家",
	LogRegTimeout:       "[tunnel] 注册超时，重试 %d/%d...",
	LogRegFailed:        "注册失败（重试%d次）",
	LogAuthSent:         "[tunnel] 已发送认证响应，等待服务器确认...",
	LogReadConsecFail:   "[tunnel] 服务端连接读取连续失败 %d 次，退出: %v",
	LogTUNWriteFail:     "[tunnel] TUN 写入失败: %v",
	LogPeerAddrChange:   "[tunnel] 玩家地址变更: %s (%s) %s → %s",
	LogNewPeer:          "[tunnel] 新玩家: %s (%s)",
	LogPeerLeave2:       "[tunnel] 玩家离开: %s (%s)",
	LogTUNConsecFail:    "[tunnel] TUN 设备读取连续失败 %d 次，退出: %v",
}

// enStrings is the English language pack.
var enStrings = &Strings{
	ServerBanner:      "▎         GameTunnel Server LAN Gaming Tunnel              ▎",
	ServerAddr:        "Address",
	ServerSubnet:      "Subnet",
	ServerMaxPlayers:  "Max Players",
	ServerAuth:        "Auth",
	ServerVersion:     "Version",
	ServerStatusPage:  "Status",
	ServerNoAuth:      "None",
	ServerHMACAuth:    "HMAC (Room-based)",
	ServerSignal:      "Received signal %v, shutting down...",
	ServerShutdown:    "Server stopped",
	ServerStartFail:   "Failed to start: %v",
	ServerInstRunning: "Another instance is running: %v\nPlease stop the existing GameTunnel server first.",
	ServerSubnetFail:  "Failed to parse subnet %s: %v",
	ServerSubnetMust:  "Subnet must be /24 (current: /%d)",
	ServerSendFail:    "[server] Send failed (total %d): %v",
	ServerStatusLog:   "[status] Status page: http://%s",
	ServerStatusFail:  "[status] HTTP server failed to start: %v",
	ServerTmplFail:    "[status] Template render failed: %v",
	ServerPeerLeave:   "[-] %s (%s) timed out",

	StatusTitle:       "GameTunnel Server",
	StatusPlayers:     "Players",
	StatusUptime:      "Uptime",
	StatusVersion:     "Version",
	StatusNoPlayers:   "No players connected",
	StatusTablePlayer: "Player",
	StatusTableVIP:    "Virtual IP",
	StatusTableAddr:   "Address",
	StatusTablePing:   "Ping",
	StatusTableIdle:   "Idle",
	StatusAuthHMAC:    "HMAC Auth",
	StatusAuthNone:    "No Auth",
	StatusJustNow:     "just now",
	StatusSecAgo:      "%ds ago",

	TrayTitle:          "GameTunnel",
	TrayTooltip:        "GameTunnel - Disconnected",
	TrayStatusOffline:  "🔴 Disconnected",
	TrayConnect:        "⚡ Connect",
	TrayConnectDesc:    "Connect to server",
	TrayDisconnect:     "🔌 Disconnect",
	TrayDisconnectDesc: "Disconnect current session",
	TraySettings:       "⚙ Settings...",
	TraySettingsDesc:   "Configure server and player info",
	TrayViewLog:        "📄 View Log",
	TrayOpenLogFile:    "Open log file",
	TrayQuit:           "❌ Quit",
	TrayQuitDesc:       "Quit GameTunnel",
	TrayConnecting:     "🟡 Connecting...",
	TrayTooltipConn:    "GameTunnel - Connecting...",
	TrayStatusOnline:   "🟢 %s · %d online",
	TrayTooltipOnline:  "GameTunnel - %s · %d online",
	TrayCfgUpdated:     "[tray] Config updated",
	TrayNoServer:       "Please configure server address first",
	TrayStatusError:    "🔴 Connection failed",
	TrayEditConfig:     "📝 Edit Config",
	TrayEditConfigDesc: "Open config file in Notepad",

	DlgTitle:      "GameTunnel Settings",
	DlgServerAddr: "Server:",
	DlgPlayerName: "Name:",
	DlgRoomID:     "Room:",
	DlgPassword:   "Password:",
	DlgOK:         "OK",
	DlgCancel:     "Cancel",
	DlgStatusIdle: "Disconnected",
	DlgStatusConn: "Connected · %s · %d online",
	DlgFirstRun:   "First-Time Setup - GameTunnel",
	DlgConnect:    "Connect",
	DlgShowPass:   "Show password",
	DlgInvalidAddr: "Invalid server address, should be IP:port",

	AppAutoConnect:   "[app] Auto-connecting to %s",
	AppSaveFail:      "[app] Failed to save config: %v",
	AppDisconnected:  "[app] Disconnected",
	AppDisconnectErr: "[app] Disconnected: %v",

	ConnErrTitle:        "Connection Failed",
	ConnErrRetry:        "Retry",
	ConnErrSettings:     "Edit Settings",
	ConnErrStop:         "Stop",
	ConnErrBalloon:      "GameTunnel is running. Click the tray icon in the bottom-right to set up.",
	ConnErrBalloonTitle: "GameTunnel",
	FirstRunBalloon:     "GameTunnel started! Click the tray icon in the bottom-right for first-time setup.",
	DlgNameEmpty:        "Player name cannot be empty",

	RunStartup: "=== GameTunnel Started ===",

	CfgHeader:     "# GameTunnel Configuration",
	CfgServerHint: "# Server address (required, e.g. 1.2.3.4:4700 or [2408::1]:4700)",
	CfgNameHint:   "# Player name (default: computer name)",
	CfgRoomHint:   "# Room ID (default: default)",
	CfgPassHint:   "# Password (leave empty if none)",

	KickInvalidName:   "Invalid username",
	KickInvalidRoom:   "Invalid room ID",
	KickRateLimit:     "Registration too frequent, please try again later",
	KickIPLimit:       "Too many connections from this IP",
	KickAuthPending:   "Authentication in progress, please wait",
	KickRoomFull:      "Room is full",
	KickDuplicateName: "A player with the same name already exists in this room, please change your name",
	KickServerBusy:    "Server is busy, please try again later",
	KickIPExhausted:   "No available IP addresses",
	KickInternalError: "Internal server error",
	KickAuthAbnormal:  "Invalid authentication state",
	KickAuthTimeout:   "Authentication timed out",
	KickWrongPassword: "Wrong password",
	LogPlayerJoin:     "[+] %s (%s) → %s  [Online: %d]",
	LogChallengeFail:  "[auth] Failed to generate challenge: %v",
	LogAuthFail:       "[auth] Authentication failed: %s (%s)",
	LogAuthPass:       "[auth] Authentication passed: %s (%s)",
	LogPlayerLeave:    "[-] %s (%s) disconnected",

	ErrInvalidServer:    "Invalid server address: %w",
	ErrBindUDP:          "Failed to bind UDP: %w",
	ErrRegisterFailed:   "Registration failed: %w",
	ErrTooManyAuth:      "Authentication failed: server sent too many auth requests",
	ErrRejected:         "Rejected: %s",
	ErrNeedPassword:     "Server requires a room password, use -password flag",
	ErrCreateTUN:        "Failed to create TUN: %w",
	ErrDecodeFailed:     "Failed to decode response: %w",
	ErrParseIPFailed:    "Failed to parse IP assignment: %w",
	ErrParseAuthFailed:  "Failed to parse auth challenge: %w",
	ErrDeriveKeyFailed:  "Failed to derive auth key",
	ErrElevateFailed:    "  Failed to elevate: %v\n",
	LogReuseTUN:         "[tunnel] Reusing TUN device (IP %s unchanged)",
	LogIPChanged:        "[tunnel] IP changed %s → %s, recreating TUN device",
	LogPeerExit:         "[tunnel] %s exited, disconnecting",
	LogTunnelDisconnect: "[tunnel] Disconnected",
	LogSendFail:         "[tunnel] Send failed (total %d): %v",
	LogP2PSuccess:       "[tunnel] P2P hole punch succeeded (phase %d): %s",
	LogP2PFailed:        "[tunnel] P2P hole punch completed (not confirmed direct), will relay: %s",
	LogServerTimeout:    "[tunnel] Server unresponsive for over %v, connection may be lost",
	LogCleanPeer:        "[tunnel] Cleaning stale peer: %s (%s) — unresponsive for %v",
	LogRetryPunch:       "[tunnel] Retrying hole punch: %d undirect peers",
	LogRegTimeout:       "[tunnel] Registration timeout, retry %d/%d...",
	LogRegFailed:        "Registration failed (retried %d times)",
	LogAuthSent:         "[tunnel] Auth response sent, waiting for server confirmation...",
	LogReadConsecFail:   "[tunnel] Server read failed %d times consecutively, exiting: %v",
	LogTUNWriteFail:     "[tunnel] TUN write failed: %v",
	LogPeerAddrChange:   "[tunnel] Peer address changed: %s (%s) %s → %s",
	LogNewPeer:          "[tunnel] New peer: %s (%s)",
	LogPeerLeave2:       "[tunnel] Peer left: %s (%s)",
	LogTUNConsecFail:    "[tunnel] TUN read failed %d times consecutively, exiting: %v",
}

var current *Strings = zhStrings // default to Chinese for backward compatibility

// Set switches the active language.
func Set(lang Lang) {
	switch lang {
	case EN:
		current = enStrings
	case ZH:
		current = zhStrings
	default:
		current = zhStrings
	}
}

// S returns the current language strings.
func S() *Strings {
	return current
}

// T is a convenience shorthand: i18n.T().FieldName
func T() *Strings {
	return current
}

// ParseLang parses a language string and returns the Lang constant.
// Returns ZH for unknown values (backward compatible default).
func ParseLang(s string) Lang {
	switch s {
	case "en", "EN", "english", "English":
		return EN
	case "zh", "ZH", "chinese", "Chinese", "中文":
		return ZH
	default:
		return ZH
	}
}

// Format is a convenience wrapper around fmt.Sprintf for translated strings.
func Format(template string, args ...interface{}) string {
	if len(args) == 0 {
		return template
	}
	return fmt.Sprintf(template, args...)
}
