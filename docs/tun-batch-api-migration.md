# wireguard/tun 批量 I/O API 迁移记录

> 2026-05-08 修复，基于 commit `15be643`。
> 修复在 `fbf8bf0` 中提交。

## 一、问题描述

编译客户端时出现以下错误：

```
# github.com/holipay/gametunnel/internal/tun
internal/tun/tun.go:81:28: not enough arguments in call to d.tunDev.Read
 have ([]byte, number)
 want ([][]byte, []int, int)
internal/tun/tun.go:86:24: cannot use data (variable of type []byte) as [][]byte value in argument to d.tunDev.Write
```

## 二、根因分析

`golang.zx2c4.com/wireguard/tun` 库在 `v0.0.0-20250521234502` 版本中将 `tun.Device` 接口从单包 I/O 改为批量 I/O：

### 旧接口（单包）

```go
type Device interface {
    Read(buf []byte, offset int) (int, error)
    Write(data []byte, offset int) (int, error)
    // ...
}
```

### 新接口（批量）

```go
type Device interface {
    Read(bufs [][]byte, sizes []int, offset int) (int, error)
    Write(bufs [][]byte, offset int) (int, error)
    // ...
}
```

**变更原因**：批量接口允许一次系统调用处理多个数据包，提升高吞吐场景（如游戏隧道）的性能。

## 三、修复方案

`internal/tun/tun.go` 中的 `Device` 结构体已有批量 I/O 所需的字段：

```go
type Device struct {
    tunDev       tun.Device
    // ...
    readSizes    [1]int       // 已声明但未使用
    readPackets  [1][]byte    // 已声明但未使用
    writePackets [1][]byte    // 已声明但未使用
}
```

这些字段说明开发者已预见 API 变更，但 `Read`/`Write` 方法未更新。

### 修复后的代码

```go
// Read 适配批量接口：将单包调用包装为 batch[0]
func (d *Device) Read(buf []byte) (int, error) {
    d.readPackets[0] = buf                    // 将 caller 的 buf 放入 batch slice
    n, err := d.tunDev.Read(d.readPackets[:], d.readSizes[:], 0)  // 调用批量接口
    if err != nil {
        return 0, err
    }
    if n == 0 {
        return 0, nil
    }
    return d.readSizes[0], nil               // 返回实际读取的字节数
}

// Write 适配批量接口：将单包调用包装为 batch[0]
func (d *Device) Write(data []byte) (int, error) {
    d.writePackets[0] = data                  // 将 data 放入 batch slice
    n, err := d.tunDev.Write(d.writePackets[:], 0)  // 调用批量接口
    if err != nil {
        return 0, err
    }
    if n == 0 {
        return 0, nil
    }
    return len(data), nil                     // 返回写入字节数
}
```

### 设计要点

| 要点 | 说明 |
|------|------|
| 批量大小为 1 | `readPackets [1]byte` — 每次只处理一个包，与旧接口行为一致 |
| 零拷贝 | `readPackets[0] = buf` — 直接引用 caller 的 buffer，不额外分配 |
| 返回值语义 | `Read` 返回 `d.readSizes[0]`（实际读取长度），`Write` 返回 `len(data)` |
| 错误处理 | 与旧接口一致：返回 `(0, error)` |

## 四、影响范围

| 文件 | 影响 |
|------|------|
| `internal/tun/tun.go` | 直接修改（本修复） |
| `internal/client/recv.go` | 无影响 — 调用的是 `TunDevice` 接口（`Read([]byte) (int, error)`），不直接调用 `tun.Device` |
| `internal/client/tunnel.go` | 无影响 — 同上 |
| 测试文件 | 无影响 — mock 实现的是 `TunDevice` 接口 |

`TunDevice` 接口（`internal/client/tunnel.go`）保持不变：

```go
type TunDevice interface {
    Read(buf []byte) (int, error)
    Write(data []byte) (int, error)
    Close() error
}
```

`internal/tun/tun.go` 的 `Device` 实现了这个接口，是 `tun.Device`（wireguard）和 `TunDevice`（client）之间的适配层。

## 五、未来考虑

当前使用批量大小 1，性能与旧接口相同。如果未来需要更高吞吐，可以：

1. 增大 `readPackets`/`writePackets` 数组大小（如 32）
2. 在 `receiveFromTUN` 中批量读取多个包
3. 批量发送到服务器（减少 UDP 系统调用次数）

但对于局域网游戏场景（< 1000 pps），单包处理已足够。
