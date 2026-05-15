# 客户端鲁棒性修复与 TUN 路由改造记录

> 2026-05-09，基于 commit `98eb724`。
> 修复分四批提交：`8093241`、`70e87ad`、`ddc7f37`、`36a0fe4`。

---

## 一、背景

客户端 `cmd/client` 编译失败，随后进行了全面的代码审查，发现并修复了编译错误、运行时缺陷、打洞鲁棒性问题，并完成了 TUN 路由架构改造，使游戏流量真正通过虚拟网卡走隧道。

---

## 二、编译错误修复（commit `8093241`）

### 问题 1：`IDOK` / `IDCANCEL` 未定义

**文件**：`cmd/client/dialog_windows.go`
**严重度**：🔴 编译失败

**原因**：Win32 标准对话框常量 `IDOK`（1）和 `IDCANCEL`（2）在 const 块中遗漏。

**修复**：在 const 块末尾添加：
```go
IDOK     = 1
IDCANCEL = 2
```

### 问题 2：`constant 2160593024 overflows int32`

**文件**：`cmd/client/dialog_windows.go`
**严重度**：🔴 编译失败

**原因**：`DS_MODALFRAME|WS_POPUP|WS_CAPTION|WS_SYSMENU|DS_CENTER` = `0x80C80880`（2160593024），超出 `int32` 范围（max 2147483647）。`writeInt32` 参数类型为 `int32`。

**修复**：将 `writeInt32` 参数从 `int32` 改为 `uint32`，所有调用处同步修改。

### 问题 3：`&buf` 双重指针

**文件**：`cmd/client/dialog_windows.go`
**严重度**：🔴 编译失败

**原因**：`addItem` 等辅助函数的 `buf` 参数已是 `*bytes.Buffer`，内部调用 `writeInt32(&buf, ...)` 变成了 `**bytes.Buffer`。顶层 `buildDialogTemplate` 中 `buf` 是值类型，用 `&buf` 是正确的——两处语义不同但写法相同。

**修复**：`addItem` 内部所有 `&buf` 改为 `buf`。

### 问题 4：未使用的 `unsafe` 导入

**文件**：`cmd/client/main_windows.go`
**严重度**：🔴 编译失败

**原因**：重构后 `unsafe` 不再被直接使用（改用 `windows.UTF16PtrFromString`）。

**修复**：删除 `"unsafe"` 导入。

---

## 三、设置对话框不显示（commit `70e87ad`）

### 问题：`DS_SETFONT` 标志缺失

**文件**：`cmd/client/dialog_windows.go`
**严重度**：🔴 功能失效

**现象**：客户端运行后，点击托盘"设置"菜单，对话框不弹出。

**根因分析**：

DLGTEMPLATE 二进制格式规定：
- 如果 style 包含 `DS_SETFONT`（0x0040），模板头在标题之后跟 font size（WORD）+ font name（字符串）
- 如果不包含，标题之后直接是控件定义

代码在模板中写了字体信息：
```go
writeInt16(&buf, 9)                    // font size
writeUTF16(&buf, "Microsoft YaHei UI") // font name
```

但 style 是 `DS_MODALFRAME|WS_POPUP|WS_CAPTION|WS_SYSMENU|DS_CENTER`，没有 `DS_SETFONT`。

Windows 解析模板时，把 font 的 `9` 当作第一个控件的起始字节——整个模板结构错位，`DialogBoxIndirectParam` 返回 0（失败），代码误判为"用户点取消"。

**修复**：
1. 新增常量 `DS_SETFONT = 0x0040`
2. style 改为 `DS_MODALFRAME|DS_SETFONT|WS_POPUP|WS_CAPTION|WS_SYSMENU|DS_CENTER`
3. 添加错误日志区分"创建失败"和"用户取消"

---

## 四、打洞鲁棒性改进（commit `ddc7f37`）

对 `internal/client/` 的 hole punch 和连接逻辑进行全面审查，发现 6 个问题。

### 问题 1：`routePacket` 不检查 `DirectReach`（静默丢包）

**文件**：`internal/client/route.go`
**严重度**：🔴 数据丢失

**原因**：打洞有三个阶段（100ms/250ms/500ms 各 5 次），打完就结束。但 `routePacket` 只检查 `peer.PublicAddr != nil` 就直接 P2P 发送——此时打洞可能还没成功（NAT 映射未建立），UDP 包被对端 NAT 静默丢弃。

**修复**：增加 `peer.DirectReach.Load()` 检查，只有确认 P2P 直通后才走直接路径，否则 fallback 到服务器中继。

### 问题 2：`handleDirectData` 只校验 IP 不校验端口

**文件**：`internal/client/recv.go`
**严重度**：🟡 安全风险

**原因**：验证"包确实来自 peer 的公网地址"时，只比较了 IP，没比较端口。攻击者可以伪造源 IP 发送数据包。

**修复**：增加 `from.Port != peer.PublicAddr.Port` 检查。

### 问题 3：peer 地址变更不触发重新打洞

**文件**：`internal/client/recv.go`
**严重度**：🟡 连接中断

**原因**：NAT 重新绑定（rebinding）后 peer 的公网地址变了，但 `handlePeerInfo` 只更新 `PublicAddr`，不重置 `DirectReach`，也不重新打洞。旧的 P2P 路径已失效，数据包发到旧地址被丢弃。

**修复**：检测地址变更时重置 `DirectReach` 并重新发起打洞。

### 问题 4：无服务器存活检测

**文件**：`internal/client/keepalive.go`
**严重度**：🟡 连接感知

**原因**：`keepaliveLoop` 每 10 秒发一次 KeepAlive，但从不检查服务器是否响应。服务器崩溃后客户端无感知。

**修复**：新增 `markServerResponse()`，每次收到服务器数据时更新时间戳。`keepaliveLoop` 检查 45 秒无响应则告警。

### 问题 5：过期 peer 永远不清理

**文件**：`internal/client/keepalive.go`
**严重度**：🟡 内存泄漏

**原因**：peer 列表只增不减（除非服务器主动发 PeerInfo 移除）。peer 异常断开（崩溃、网络中断）后，其条目永远留在内存中，还会触发无效的打洞尝试。

**修复**：
- `Peer` 新增 `lastSeen` 时间戳
- `stalePeerCleanupLoop` 每 30 秒清理 90 秒未出现的 peer

### 问题 6：打洞响应无节流

**文件**：`internal/client/keepalive.go`
**严重度**：🟡 流量放大

**原因**：收到 `TypeHolePunch` 就立即回包 5 次，无频率限制。攻击者可以伪造 HolePunch 包触发放大攻击。

**修复**：`Peer` 新增 `lastPunchBack` 时间戳，同一 peer 5 秒内最多响应一次。

### 问题 7：打洞只尝试一次

**文件**：`internal/client/keepalive.go`
**严重度**：🟡 P2P 成功率

**原因**：NAT 映射会过期（典型 UDP NAT 超时 30-120 秒），首次打洞成功后如果不持续通信，映射消失，P2P 断开。代码没有重试机制。

**修复**：新增 `holePunchRetryLoop`，每 60 秒检查未直通的 peer 并重新打洞。

### 涉及文件

| 文件 | 改动 |
|---|---|
| `internal/client/tunnel.go` | `Peer` 新增 `lastSeen`/`lastPunchBack`；`Tunnel` 新增 `lastServerResponse`；`Connect()` 启动新 goroutine |
| `internal/client/recv.go` | 端口校验；地址变更检测 + 重新打洞；`markServerResponse` 调用 |
| `internal/client/route.go` | `DirectReach` 检查 + 中继 fallback |
| `internal/client/keepalive.go` | 服务器存活检测；过期 peer 清理；打洞重试；打洞限速 |

---

## 五、TUN 路由架构改造（commit `36a0fe4`）

### 背景

TUN 虚拟网卡配置了子网路由（如 `10.10.0.0/24`），只捕获子网内流量。游戏服务器在互联网上（如 `103.231.234.5:27015`），不匹配子网路由，流量直接走物理网卡——**完全绕过了隧道**。

### 改造方案

在 `configure()` 中新增两步：

**Step 8：隧道服务器排除路由**
```
route add <服务器公网IP> mask 255.255.255.255 <物理网关> metric 0
```
隧道自身的 UDP 流量必须走物理网卡，否则会回环进 TUN。`metric=0` 确保优先于默认路由。

**Step 9：默认路由走 TUN**
```
route add 0.0.0.0 mask 0.0.0.0 <TUN IP> metric 1
```
所有非子网流量通过 TUN，由客户端决定走 P2P 还是中继。`metric=1` 确保优先于物理网卡（其 metric 通常 ≥ 10）。

### 最终路由表

| 目标 | 网关 | Metric | 用途 |
|---|---|---|---|
| `0.0.0.0/0` | TUN IP | 1 | 捕获所有流量 |
| `<服务器IP>/32` | 物理网关 | 0 | 隧道自身流量排除 |
| `10.10.0.0/24` | TUN IP | 1 | 子网内通信 |
| `255.255.255.255` | TUN IP | 1 | 游戏广播 |
| `224.0.0.251` | TUN IP | 1 | mDNS |
| 子网广播 | TUN IP | 1 | 如 `10.10.0.255` |

### 流量路径

```
改造前：
  游戏进程 → 物理网卡 → 互联网（TUN 只收子网内流量）

改造后：
  游戏进程 → TUN虚拟网卡 → 客户端读取 → 封装UDP → 物理网卡 → 互联网
                                  ↑
                      隧道服务器IP排除在外（物理网卡直连）
```

### 涉及文件

| 文件 | 改动 |
|---|---|
| `internal/tun/tun.go` | `Config` 新增 `ServerPublicIP`；`Device` 新增 `serverPublicIP`/`physicalGateway`；`Close()` 调用 `CleanupRoutes()` |
| `internal/tun/configure.go` | 新增 Step 8/9；`detectPhysicalGateway()`；`CleanupRoutes()` |
| `cmd/client/main_windows.go` | `parseHostIP()` 解析服务器地址；传递 `ServerPublicIP` 到 TUN 配置 |

### 注意事项

- `CleanupRoutes()` 在 `Device.Close()` 时自动清理所有路由，避免残留
- 服务器排除路由必须在默认路由之前添加（Step 8 先于 Step 9）
- 物理网关通过 PowerShell `Get-NetRoute` 自动检测
- 如果检测失败，跳过排除路由（日志告警），默认路由仍会添加

---

## 六、提交汇总

| Commit | 说明 | 文件数 |
|---|---|---|
| `8093241` | 编译错误修复（IDOK/IDCANCEL、int32 溢出、&buf、unused import） | 2 |
| `70e87ad` | 设置对话框不显示（DS_SETFONT 缺失） | 1 |
| `ddc7f37` | 打洞鲁棒性（DirectReach、端口校验、地址变更、存活检测、过期清理、重试、限速） | 4 |
| `36a0fe4` | TUN 路由改造（默认路由 + 服务器排除 + 路由清理） | 3 |
