# 网卡 Metric 配置：从 PowerShell 迁移到 IP Helper API

> 2026-05-09 优化，基于 commit `b6d6fcb`。
> 实现在 `015ba4d`（重构）和 `aa49b7a`（修复）中提交。

## 一、问题背景

客户端需要让 TUN 虚拟网卡成为广播路由的首选接口。为此必须：

1. 将 TUN 接口的 metric 设为最低（1）
2. 将物理网卡的 metric 提高（100）
3. **禁用所有网卡的 AutomaticMetric**，防止 Windows 覆盖手动值

原方案通过 PowerShell `Set-NetIPInterface` 实现：

```go
// 旧方案
psMetric := fmt.Sprintf(
    "Set-NetIPInterface -InterfaceAlias '%s' -AutomaticMetric Disabled -InterfaceMetric 1 -ErrorAction SilentlyContinue",
    d.name)
RunCmd("powershell", "-NoProfile", "-Command", psMetric)
```

## 二、原方案的失败模式

| 问题 | 原因 | 影响 |
|------|------|------|
| PowerShell 调用慢 | 每次启动新进程，200-500ms/次 | 配置阶段耗时 2-3 秒 |
| 静默失败 | `-ErrorAction SilentlyContinue` 吞掉所有错误 | 设置失败了也不知道 |
| wintun 接口未就绪 | 驱动刚创建时 PowerShell 可能识别不到接口 | 首次运行大概率失败 |
| 物理 NIC metric 回退 | 只设了 TUN 的 metric，物理 NIC 的 AutomaticMetric 被 Windows NLA 服务重新启用 | 几秒后 metric 恢复默认，广播路由失效 |
| 执行策略限制 | 部分企业环境禁止运行 PowerShell 脚本 | 直接无法工作 |
| `verifyMetric` 检查错误字段 | 检查 `InterfaceMetric`（值）而非 `AutomaticMetric`（根因） | 即使值正确，AutomaticMetric 仍可能为 Enabled |

## 三、解决方案：IP Helper API

直接调用 `iphlpapi.dll` 的 `SetIpInterfaceEntry`，在内核级别禁用 `AutomaticMetric`。

### 3.1 核心函数

> **注**: 以下代码已更新为当前实现。原始版本使用 `SetIpInterfaceEntry` IP Helper API，但因 wintun 虚拟适配器返回 `ret=87`，改为 `netsh` 命令行方式。

```go
// setMetricAPI 通过 netsh 禁用指定网卡的 AutomaticMetric 并设置 metric 值。
func setMetricAPI(ifIndex uint32, luid uint64) error {
    name, err := findAdapterNameByIndex(ifIndex)
    if err != nil {
        return fmt.Errorf("find adapter name: %w", err)
    }

    if err := RunCmd("netsh", "interface", "ip", "set", "interface",
        fmt.Sprintf("name=%s", name), "metric=1"); err != nil {
        return fmt.Errorf("netsh set metric: %w", err)
    }

    log.Printf("[tun] AutomaticMetric disabled via netsh: %s (idx=%d)", name, ifIndex)
    return nil
}
```

### 3.2 MIB_IPINTERFACE_ROW 结构体

结构体字段顺序严格匹配 Windows SDK `netioapi.h`，关键字段：

```
offset 0:  Family              (uint16)
offset 8:  InterfaceLuid       (uint64)
offset 16: InterfaceIndex      (uint32)
offset 56: UseAutomaticMetric  (int32)  ← 0=手动, 1=自动
```

> ⚠️ 不要随意调整字段顺序或删减字段，否则偏移量错误会导致 `SetIpInterfaceEntry` 写坏内存。

### 3.3 网卡枚举

通过 `GetAdaptersAddresses` 枚举所有网卡，按 `FriendlyName` 匹配：

```go
procGetAdaptersAddresses.Call(
    uintptr(syscall.AF_INET),
    uintptr(gaaSkipUnicast|gaaSkipAnycast|gaaSkipMulticast|gaaSkipDNS),
    0,
    uintptr(unsafe.Pointer(&buf[0])),
    uintptr(unsafe.Pointer(&bufLen)),
)
```

`FriendlyName` 是 `*uint16` 类型，用 `golang.org/x/sys/windows.UTF16PtrToString` 转换。

> ⚠️ 标准库 `syscall` 包中没有 `UTF16PtrToString`，必须用 `golang.org/x/sys/windows` 包。

## 四、新方案的配置流程

```
configure()
  │
  ├─ Step 1: netsh 分配静态 IP（不变）
  │
  ├─ Step 2: 禁用 AutomaticMetric
  │   ├─ 优先: IP Helper API (~5ms)
  │   └─ 回退: PowerShell (如果 API 不可用)
  │
  ├─ Step 3: 验证 + 重试
  │   ├─ 检查 UseAutomaticMetric == 0（根因，不是 metric 值）
  │   └─ 失败则等 500ms 重试一次
  │
  └─ Step 4-8: route add + 防火墙（不变）
```

**关键改进**：不仅设置 TUN 的 `AutomaticMetric=Disabled`，还同时禁用所有物理网卡的 `AutomaticMetric`。这是原方案遗漏的——物理 NIC 的 AutomaticMetric 会被 Windows NLA 服务在几秒内重新启用。

## 五、对比

| 维度 | 旧方案 (PowerShell) | 新方案 (IP Helper API) |
|------|-------------------|----------------------|
| 速度 | 200-500ms/次 | ~5ms/次 |
| 可靠性 | 静默失败 | 直接返回 errno |
| 执行策略 | 受限 | 无影响 |
| 验证逻辑 | 检查 metric 值（会被覆盖） | 检查 AutomaticMetric 标志（根因） |
| 物理 NIC | 只设 metric 值 | 同时禁用 AutomaticMetric |
| 回退 | 无 | API 失败自动回退 PowerShell |

## 六、改动文件

| 文件 | 变化 |
|------|------|
| `internal/tun/metric_windows.go` | 新增：IP Helper API 封装 |
| `internal/tun/configure.go` | 新增：API 优先的配置流程 |
| `internal/tun/tun.go` | 删除：`configure()`、`verifyMetric()`、`raisePhysicalNICMetrics()` |
