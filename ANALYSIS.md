# GameTunnel 代码分析与优化报告

## 项目概览

- **语言**: Go 1.25
- **架构**: UDP 隧道服务器 (Linux) + TUN 客户端 (Windows/wintun)
- **协议**: 自定义二进制协议，CRC32 校验，HMAC-SHA256 认证
- **代码量**: ~6 个 Go 文件，约 1200 行

---

## 🔴 严重问题 (4个)

### 1. 广播重复投递

**位置**: `cmd/client/main.go` → `relayBroadcast()`

**问题**: 客户端同时向服务器和所有 P2P 对端发送广播包，而服务器收到后又转发给所有客户端。结果每个 peer 收到同一广播包 2~3 次。

**影响**: 浪费带宽，可能导致游戏逻辑异常（重复的局域网发现包）。

**修复**: P2P 直连的 peer 不再通过服务器中继广播，或客户端只向服务器发送广播（由服务器转发）。

### 2. handleRegister 锁模式危险

**位置**: `cmd/server/main.go` → `handleRegister()` / `registerClient()`

**问题**: `s.mu.Lock()` 在 `handleRegister` 获取，但释放散落在 `registerClient` 内部和其他分支中。跨函数边界的 lock/unlock 模式极易导致死锁或 panic。

**影响**: 潜在的死锁、panic（重复 unlock）、竞态条件。

**修复**: 统一锁模式，用 defer 或明确的函数边界管理锁。

### 3. handleAuthResponse 竞态窗口

**位置**: `cmd/server/main.go` → `handleAuthResponse()`

```go
// 问题代码：RLock 释放后到 Lock 之间有竞态窗口
s.mu.RLock()
c := s.addrMap[from.String()]
s.mu.RUnlock()
// ← 另一个 goroutine 可能在这里修改 addrMap
s.mu.Lock()
delete(s.addrMap, from.String())
s.mu.Unlock()
```

**修复**: 统一使用 `s.mu.Lock()` 进行读-改-写操作。

### 4. 认证洪水攻击（无上限）

**位置**: `cmd/server/main.go` → `sendAuthChallenge()`

**问题**: 未认证连接数量没有限制。攻击者可伪造大量注册请求耗尽内存。

**修复**: 添加 pending auth 计数器和上限。

---

## 🟡 中等问题 (4个)

### 5. Rate Limiter 竞态

`checkRate` 用独立 `rateMu`，但 `count++` 和 `count <= rateLimit` 之间不是原子操作。

### 6. teeWriter 吞掉文件写入错误

```go
func (t *teeWriter) Write(p []byte) (n int, err error) {
    t.a.Write(p)  // 错误被忽略
    t.b.Write(p)
    return len(p), nil
}
```

### 7. 密码明文存储

`%APPDATA%\GameTunnel\config.json` 中房间密码以明文保存。

### 8. HMAC Key 冗余存储

每个 pending client 都存一份 `s.authKey`（服务器全局共享的 key），浪费内存。

---

## 🟢 优化建议 (4个)

### 9. IP 分配效率

`nextAvailableIP()` 线性扫描 .2~.254。可改为空闲 IP 集合。

### 10. 客户端无主动断开通知

客户端退出时不向服务器发送断开消息，依赖 45 秒超时清理。

### 11. 无加密

游戏数据明文传输。CRC32 只防意外损坏，不防窥探。对游戏隧道可接受。

### 12. 无 MTU 分片

协议添加了额外开销（2+4+8 字节）但未考虑分片。
