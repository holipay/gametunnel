# AutomaticMetric 与 LUID 查找修复

> 2026-05-16，基于 commit `dbc3232`。
> 修复在 PR #14 (`d6a9b25`) 和 PR #15 (`a7c26ea`) 中提交。

## 一、问题现象

Windows 客户端 TUN 适配器配置阶段，IP Helper API 调用失败，回退到 PowerShell：

```
2026/05/16 14:33:07 [tunnel] 已发送认证响应，等待服务器确认...
2026/05/16 14:33:07 Creating adapter
2026/05/16 14:33:08 [tun] TUN adapter: idx=12 luid=14918723521478656
2026/05/16 14:33:08 [tun] IP Helper API failed (set TUN: SetIpInterfaceEntry(idx=12): ret=87 err=The operation completed successfully.), trying PowerShell
2026/05/16 14:33:16 [tun] TUN adapter: idx=12 luid=14918723521478656
2026/05/16 14:33:21 [tun] WARNING: could not disable AutomaticMetric, broadcast routing may be affected
2026/05/16 14:33:22 [tun] detected physical gateway: 192.168.1.1
2026/05/16 14:33:24 [tun] configured: IP=10.10.0.2/24, subnet route only (no default route)
```

**影响**：
- 每次 TUN 配置耗时 ~16 秒（正常应 <2 秒）
- AutomaticMetric 可能未被禁用，广播路由不稳定
- 星际争霸等依赖 UDP 广播的局域网游戏可能出现重复房间或发现失败

## 二、根因分析

### Bug 1：`findAdapter` 静默返回 LUID=0

**文件**：`internal/tun/metric_windows.go` → `findAdapter()`

**代码**（修复前）：

```go
// 找到适配器后，通过 GetIpInterfaceEntry 读取 LUID
r1, _, _ := procGetIpInterfaceEntry.Call(uintptr(unsafe.Pointer(&row[0])))
if r1 == 0 {
    luid = binary.LittleEndian.Uint64(row[offsetInterfaceLuid:])
    return p.IfIndex, luid, nil
}
return p.IfIndex, 0, nil   // ← 问题：LUID=0 但返回 nil error
```

**因果链**：
1. TUN 适配器刚创建，IP 接口条目尚未就绪
2. `GetAdaptersAddresses` 按名称找到适配器（idx=12 ✅）
3. `GetIpInterfaceEntry` 失败（适配器未在 IP 栈中注册），返回非零 ret
4. 代码忽略错误，返回 `(idx=12, luid=0, nil)`
5. 上层 `setMetricAPI(idx=12, luid=0)` 用全零 LUID 调用 `GetIpInterfaceEntry` → `ret=87`

### Bug 2：`setMetricAPI` 缺少 `InitializeIpInterfaceEntry`

**文件**：`internal/tun/metric_windows.go` → `setMetricAPI()`

**代码**（修复前）：

```go
func setMetricAPI(ifIndex uint32, luid uint64) error {
    row := make([]byte, mibRowSize)

    // 仅设置 3 个字段，其余保持零值
    binary.LittleEndian.PutUint16(row[offsetFamily:], syscall.AF_INET)
    binary.LittleEndian.PutUint64(row[offsetInterfaceLuid:], luid)
    binary.LittleEndian.PutUint32(row[offsetInterfaceIndex:], ifIndex)

    r1, _, _ := procGetIpInterfaceEntry.Call(...)  // 填充整行
    binary.LittleEndian.PutUint32(row[offsetUseAutoMetric:], 0)
    r1, _, _ = procSetIpInterfaceEntry.Call(...)   // ret=87 ❌
}
```

**根因**：`MIB_IPINTERFACE_ROW` 是一个 400+ 字节的复杂结构体，内部有大量依赖 Windows 版本的字段。微软文档要求的正确流程是：

1. `InitializeIpInterfaceEntry(&row)` — 将所有字段初始化为合法默认值
2. 设置目标 `InterfaceLuid` / `InterfaceFamily`
3. `GetIpInterfaceEntry(&row)` — 读取当前配置
4. 修改目标字段
5. `SetIpInterfaceEntry(&row)` — 写回

从全零缓冲区直接调用 `GetIpInterfaceEntry`，某些内部字段（如 `SitePrefixLength`、`CompartmentId`）处于无效状态，`SetIpInterfaceEntry` 拒绝写入。

## 三、修复方案

### 修复 1：`findAdapter` 返回错误（PR #14）

```diff
- return p.IfIndex, 0, nil
+ return 0, 0, fmt.Errorf("GetIpInterfaceEntry(idx=%d): adapter found but not ready", p.IfIndex)
```

上层 `applyMetricAPI()` 收到错误后走 PowerShell 回退，`configure()` Step 3 的重试逻辑也会正确触发第二次 `applyMetricAPI()`（此时适配器已就绪）。

### 修复 2：`setMetricAPI` 加入 `InitializeIpInterfaceEntry`（PR #15）

```diff
+ procInitializeIpInterfaceEntry = modIphlpapi.NewProc("InitializeIpInterfaceEntry")

  func setMetricAPI(ifIndex uint32, luid uint64) error {
      row := make([]byte, mibRowSize)
+
+     // 将整行初始化为默认值，避免 SetIpInterfaceEntry 因内部字段无效返回 ret=87
+     r1, _, _ := procInitializeIpInterfaceEntry.Call(uintptr(unsafe.Pointer(&row[0])))
+     if r1 != 0 {
+         return fmt.Errorf("InitializeIpInterfaceEntry: ret=%d", r1)
+     }

      binary.LittleEndian.PutUint16(row[offsetFamily:], syscall.AF_INET)
      binary.LittleEndian.PutUint64(row[offsetInterfaceLuid:], luid)
      // ...
```

## 四、修复后效果

| 指标 | 修复前 | 修复后 |
|------|--------|--------|
| TUN 配置耗时 | ~16 秒（PowerShell 回退） | <2 秒（IP Helper API 直接成功） |
| SetIpInterfaceEntry | ret=87 失败 | 成功 |
| AutomaticMetric | 可能未禁用 | 可靠禁用 |
| 广播路由（255.255.255.255） | metric 不确定 | TUN metric=1，优先走 TUN |
| 同局域网玩家房间发现 | 可能出现重复房间 | 正常，仅 TUN 路径 |

## 五、提交记录

| Commit | PR | 内容 |
|--------|-----|------|
| `1f462ad` | #14 | fix: `findAdapter` 返回 error 而非 `(idx, 0, nil)` |
| `32c2477` | #15 | fix: `setMetricAPI` 加入 `InitializeIpInterfaceEntry` |
| `d6a9b25` | #14 merge | squash merge |
| `a7c26ea` | #15 merge | squash merge |

## 六、技术参考

- [InitializeIpInterfaceEntry (Microsoft Docs)](https://learn.microsoft.com/en-us/windows/win32/api/netioapi/nf-netioapi-initializeipinterfaceentry)
- [SetIpInterfaceEntry (Microsoft Docs)](https://learn.microsoft.com/en-us/windows/win32/api/netioapi/nf-netioapi-setipinterfaceentry)
- [MIB_IPINTERFACE_ROW structure](https://learn.microsoft.com/en-us/windows/win32/api/nldef/ns-nldef-mib_ipinterface_row)
- Windows SDK 10.0.26100.0 `netioapi.h` — 结构体字段偏移量来源
