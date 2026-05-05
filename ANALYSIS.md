# GameTunnel 代码分析与优化报告

## 项目概览

- **语言**: Go 1.22
- **架构**: UDP 隧道服务器 (Linux) + TUN 客户端 (Windows/wintun)
- **协议**: 自定义二进制协议，CRC32 校验，HMAC-SHA256 认证
- **代码量**: ~6 个 Go 文件，约 1200 行

---

## ✅ 已修复问题

### 1. HMAC Key 派生使用硬编码 room ID（严重）

**原问题**: 服务端用 `DeriveKey(password, "default")` 派生密钥，客户端用实际 room ID。非 "default" 房间认证必失败。

**修复**: 移除服务端全局 `authKey`，改为在 `handleAuthResponse` 中用客户端的 room ID 实时派生密钥。`sendAuthChallengeLocked` 保存 `authRoomID` 到 pending 客户端状态。

### 2. go.mod 声明不存在的 Go 版本（严重）

**原问题**: `go 1.25.0` 不存在，导致无法编译。

**修复**: 改为 `go 1.22`。

### 3. 预编译二进制动态链接 glibc（严重）

**原问题**: Release 二进制依赖 GLIBC 2.34，在低版本系统（如 Ubuntu 20.04 的 GLIBC 2.31）上无法运行。

**修复**: Makefile 中服务端编译添加 `CGO_ENABLED=0`，生成静态链接二进制。install.sh 增加源码编译回退。

### 4. 跨函数锁模式（中等）

**原问题**: `handleRegister` 获取 `s.mu.Lock()`，但释放在 `registerClientLocked` / `sendAuthChallengeLocked` 内部。虽然函数名有 "Locked" 后缀，但模式脆弱。

**状态**: 保持现有模式（命名约定已明确），但添加了注释说明锁的所有权。

### 5. 客户端退出不通知服务器（中等）

**原问题**: 客户端 `Disconnect()` 只关闭 conn，服务器需等 45 秒超时清理。

**修复**: 
- 协议新增 `TypeDisconnect (0x0B)` 消息类型
- 客户端 `Disconnect()` 在关闭 conn 前发送 TypeDisconnect
- 服务端新增 `handleDisconnect()` 立即清理并广播 PeerInfo 更新

### 6. Rate Limiter 竞态（低）

**原问题**: `checkRate` 中 `count++` 和 `count <= limit` 分开读取。

**修复**: 在锁内直接返回比较结果。

### 7. teeWriter 吞掉写入错误（低）

**原问题**: `Write()` 返回 `len(p)` 即使文件写入失败。

**修复**: 返回两者中较小的 `n` 值。

### 8. install.sh 缺少端口检查（低）

**修复**: 安装前检查 UDP 端口是否被占用。预编译二进制不可用时自动回退到源码编译。

---

## 🟡 已知限制（非 bug，可接受）

- **无加密**: 游戏数据明文中转，CRC32 仅防损坏。建议配合 WireGuard 使用。
- **无重放保护**: 协议无序列号，CRC32 不防重放。
- **密码明文存储**: config.json 中密码明文，文件权限 0600。
- **IP 分配线性扫描**: /24 子网最多 253 个客户端，线性扫描可接受。
- **无 MTU 分片**: 协议额外开销 14 字节，MTU 1400 安全。
