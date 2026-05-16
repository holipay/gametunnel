# 注册超时与 Metric API 修复

> 2026-05-16，基于 commit `b7fd8e8`。
> 修复在 `86f2f59` 中提交。

## 一、问题现象

Windows 客户端连接服务器时注册超时，日志如下：

```
2026/05/16 10:43:03 === GameTunnel 启动 ===
2026/05/16 10:43:03 [app] 自动连接到 111.229.82.204:4700
2026/05/16 10:43:13 [tunnel] 注册超时，重试 1/3...
2026/05/16 10:43:23 [app] 连接断开: 注册失败: %!w(*net.OpError=&{read udp4 ...})
```

## 二、排查过程

| 步骤 | 检查项 | 结果 |
|------|--------|------|
| 1 | 服务端进程是否运行 | ✅ `gtunnel-server` 正常监听 UDP 4700 |
| 2 | 服务端防火墙（iptables） | ✅ 默认策略 ACCEPT，无拦截规则 |
| 3 | 腾讯云轻量防火墙 | ✅ UDP 4700 已放行 |
| 4 | Windows 防火墙 | ✅ 关闭后仍无法连接 |
| 5 | 网络连通性（PowerShell UDP 测试） | ✅ UDP 包可到达服务器 |
| 6 | GameTunnel 客户端发包（tcpdump） | ❌ 服务器未收到任何 GameTunnel 包 |
| 7 | 代码审查 `sendUDP` → `sendLoop` | 🔍 **找到根因** |

**关键发现**：PowerShell 直接发送 UDP 包可以到达服务器，但 GameTunnel 客户端的注册包从未被发出。

## 三、根因分析

### Bug 1：注册包卡在 channel 中未发送

**文件**：`internal/client/register.go` + `internal/client/tunnel.go`

**原因**：`sendUDP()` 是非阻塞的，只将数据包放入 channel 缓冲区。真正执行 `conn.WriteToUDP()` 的是 `sendLoop` goroutine。但 `sendLoop` 在 `register()` **返回之后**才启动：

```go
// tunnel.go Connect()
t.conn = conn

err := t.register(ctx)    // ← 调用 sendUDP()，包放入 channel
// ...
go func() {
    t.sendLoop(runCtx)    // ← 这时才从 channel 消费并发送
}()
```

注册阶段调用 `sendUDP()` → 包进入 channel → 无人消费 → 包永远不会发出 → 10 秒超时。

### Bug 2：IP Helper API 调用时机过早

**文件**：`internal/tun/metric_windows.go` + `internal/tun/configure.go`

**原因**：TUN 适配器刚创建后立即调用 `SetIpInterfaceEntry()`，此时适配器尚未完全初始化，API 返回 `ERROR_INVALID_PARAMETER` (87)。

## 四、修复方案

### 修复 1：注册阶段直接写 UDP socket

将 `register.go` 中的 3 处 `sendUDP()` 改为 `writeUDP()`：

```diff
- t.sendUDP(packet, t.serverAddr)
+ t.writeUDP(packet, t.serverAddr)
```

`writeUDP()` 直接调用 `conn.WriteToUDP()`，不经过 channel，确保注册包立即发出。

**改动位置**：
- 第 33 行 — 首次发送注册包
- 第 54 行 — 超时重试时重新发送
- 第 153 行 — 发送 HMAC 认证响应

### 修复 2：IP 分配后延迟再调用 Metric API

在 `configure()` 中，IP 分配完成后等待 1 秒再调用 IP Helper API，重试前等待 2 秒：

```diff
  // ── Step 2: 禁用 AutomaticMetric ──
+ // 等待 TUN 适配器完全初始化
+ time.Sleep(1 * time.Second)
  if err := d.applyMetricAPI(); err != nil {
      // ...
  }
  // ── Step 3: 验证 + 重试 ──
  if !checkAutoMetricDisabled(d.name) {
-     time.Sleep(500 * time.Millisecond)
+     time.Sleep(2 * time.Second)
      // ...
  }
```

### 附加修复：Makefile 服务端二进制命名

将默认 `server` target 的输出文件名从 `gtunnel-server` 改为 `gtunnel-server-linux-amd64`，与交叉编译 target 命名一致。

```diff
- SERVER := $(BINARY_DIR)/gtunnel-server
+ SERVER := $(BINARY_DIR)/gtunnel-server-linux-amd64
```

安装路径 `/usr/local/bin/gtunnel-server` 不变，systemd 服务名不受影响。

## 五、提交记录

| Commit | 内容 |
|--------|------|
| `6c237a9` | fix: registration packets not sent (sendUDP→writeUDP) + rename server binary |
| `86f2f59` | fix: add delay before IP Helper API call for TUN adapter initialization |

## 六、影响范围

- **客户端连接**：修复注册超时，客户端现在可以正常连接服务器
- **广播路由**：AutomaticMetric 设置延迟后成功率提高，星际争霸等依赖广播的游戏局域网发现应恢复正常
- **构建产物**：服务端 Linux amd64 二进制文件名变更（`gtunnel-server` → `gtunnel-server-linux-amd64`）
