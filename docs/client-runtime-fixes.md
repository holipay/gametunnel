# 客户端运行时问题排查与修复

> 2026-05-10 排查，基于 commit `015ba4d`。
> 修复在 `f96967f` 和 `166cd25` 中提交。

## 一、问题概述

编译后运行客户端，出现三个问题：

| # | 现象 | 日志关键词 |
|---|------|-----------|
| 1 | 设置对话框弹出但无内容 | `DialogBoxIndirectParam 返回 0` |
| 2 | TUN 网卡 AutomaticMetric 配置失败 | `SetIpInterfaceEntry ret=87` |
| 3 | 服务器排除路由添加失败 | `bad metric value 0` |

## 二、问题 1：设置对话框无内容

### 现象

系统托盘点击"设置"，对话框窗口弹出（标题栏可见），但内部无任何控件。

### 排查过程

#### 2.1.1 初步分析 DLGTEMPLATE

`dialog_windows.go` 中 `buildDialogTemplate` 手工构造 Win32 对话框模板（`DLGTEMPLATE` + `DLGITEMTEMPLATE` 二进制格式）。初次分析发现两个疑点：

- 控件数量声明（`cdit` 字段）为 11，但注释混乱
- `addStatic` 函数缺少 `WS_VISIBLE` 样式

**首次修复**（commit `582734c`）：
- 控件数量 11 → 9
- `addStatic` 添加 `WS_VISIBLE`

**结果**：仍然无内容。

#### 2.1.2 控件数量修正

重新逐个计数控件：

| # | 类型 | 函数 | ID |
|---|------|------|----|
| 1 | STATIC | `addStatic` | 0 |
| 2 | EDIT | `addEdit` | IDC_SERVER (1001) |
| 3 | STATIC | `addStatic` | 0 |
| 4 | EDIT | `addEdit` | IDC_NAME (1002) |
| 5 | STATIC | `addStatic` | 0 |
| 6 | EDIT | `addEdit` | IDC_ROOM (1003) |
| 7 | STATIC | `addStatic` | 0 |
| 8 | EDIT | `addEdit` | IDC_PASSWORD (1004) |
| 9 | STATIC | `addStatic` | IDC_STATUS_LABEL (1005) |
| 10 | BUTTON | `addButton` | IDOK (1) |
| 11 | BUTTON | `addButton` | IDCANCEL (2) |

**共 11 个控件，原始的 `cdit=11` 是正确的。** 首次修复错误地改成了 9。

回退控件数量为 11（commit `0430a03`），仅保留 `WS_VISIBLE` 修复。

**结果**：仍然无内容。

#### 2.1.3 深层原因分析

即使缺少 `WS_VISIBLE`，EDIT 和 BUTTON 控件本身已有 `WS_VISIBLE` 样式，应该可见。对话框完全空白说明存在更根本的问题。

可能原因：
1. **Go GC 与 Win32 API 交互**：`bytes.Buffer` 内部的 `[]byte` 可能在 `DialogBoxIndirectParam` 阻塞期间被 GC 回收或移动
2. **goroutine 线程绑定**：`showConfigDialog` 在 `go func()` 中调用，未绑定到 OS 线程，Go 调度器可能干扰 Win32 消息循环
3. **字体不可用**：`Microsoft YaHei UI` 在某些 Windows 版本上可能不可用，导致模板解析失败

### 最终修复（commit `f96967f`）

完全重写 `dialog_windows.go`：

**改动 1**：自定义 `leBuffer` 替代 `bytes.Buffer`
```go
// 旧：bytes.Buffer + binary.Write（可能触发 GC 问题）
binary.Write(buf, binary.LittleEndian, v)

// 新：直接 append 到 []byte（无反射、无接口装箱）
func (b *leBuffer) writeUint16(v uint16) {
    b.data = append(b.data, byte(v), byte(v>>8))
}
```

**改动 2**：线程绑定 + GC 保护
```go
runtime.LockOSThread()
defer runtime.UnlockOSThread()
defer runtime.KeepAlive(tmpl)
```

**改动 3**：使用通用字体
```go
// 旧：Microsoft YaHei UI（不一定存在）
writeUTF16(&buf, "Microsoft YaHei UI")

// 新：MS Shell Dlg（Windows 自动映射到系统默认字体）
buf.writeWStr("MS Shell Dlg")
// DS_SHELLFONT = DS_SETFONT | DS_FIXEDSYS
```

**改动 4**：添加诊断日志
```go
log.Printf("[dialog] template size: %d bytes", len(tmpl))
log.Printf("[dialog] WM_INITDIALOG hwnd=%x", hwnd)
```

### 备用方案（commit `166cd25`）

即使对话框修复后，增加 fallback 机制：对话框失败时自动用记事本打开 `config.ini`。

```go
if showConfigDialog(statusText) {
    // 对话框成功，保存配置
} else {
    // 对话框失败，打开 config.ini
    openConfigFile()
}
```

## 三、问题 2：SetIpInterfaceEntry 返回 ERROR_INVALID_PARAMETER

### 现象

```
[tun] IP Helper API failed (set TUN: SetIpInterfaceEntry(idx=46): ret=87
err=The operation completed successfully.), trying PowerShell
```

`ret=87` 是 `ERROR_INVALID_PARAMETER`。`err` 显示 "The operation completed successfully" 是因为 Go 的 `syscall.Errno(0)` 格式化为该文本，不代表实际成功。

### 原因

`metric_windows.go` 中的 `mibIPInterfaceRow` Go 结构体与 Windows SDK 的 `MIB_IPINTERFACE_ROW` 布局不一致。

Windows SDK 结构体（`netioapi.h`）字段顺序：
```
offset 0:   Family (uint16)
offset 8:   InterfaceLuid (uint64)
offset 16:  InterfaceIndex (uint32)
offset 20:  MaxReassemblySize (uint32)
offset 24:  InterfaceIdentifier (uint64)
...
offset 56:  UseAutomaticMetric (uint32)  ← 需要修改的字段
```

Go 结构体中存在一个 `InterfaceLuid2` 字段（offset 232），该字段在 Windows SDK 中**不存在**。这是对 SDK 结构体的错误理解，导致后续字段偏移全部错位。

### 修复

放弃 Go 结构体，改用原始字节缓冲区 + 已知偏移量：

```go
const (
    offsetFamily         = 0
    offsetInterfaceLuid  = 8
    offsetInterfaceIndex = 16
    offsetUseAutoMetric  = 56
)

func setMetricAPI(ifIndex uint32, luid uint64) error {
    row := make([]byte, 416)  // MIB_IPINTERFACE_ROW 足够大
    binary.LittleEndian.PutUint16(row[offsetFamily:], syscall.AF_INET)
    binary.LittleEndian.PutUint64(row[offsetInterfaceLuid:], luid)
    binary.LittleEndian.PutUint32(row[offsetInterfaceIndex:], ifIndex)

    procGetIpInterfaceEntry.Call(uintptr(unsafe.Pointer(&row[0])))

    binary.LittleEndian.PutUint32(row[offsetUseAutoMetric:], 0)

    procSetIpInterfaceEntry.Call(uintptr(unsafe.Pointer(&row[0])))
}
```

这些偏移量在 Windows 10/11 所有版本中保持稳定。

## 四、问题 3：route metric 0 不合法

### 现象

```
[tun] server exclusion route warning: route: bad metric value 0
```

### 原因

`configure.go` Step 8 中，服务器排除路由使用 `metric=0`：
```go
RunCmd("route", "add", serverIP, "mask", "255.255.255.255", gw, "metric", "0")
```

Windows `route add` 命令的 metric 最低值为 1，不接受 0。

### 修复

```go
// metric=0 → metric=1（仍然是最低优先级，确保优先于默认路由）
RunCmd("route", "add", serverIP, "mask", "255.255.255.255", gw, "metric", "1")
```

## 五、经验总结

| 经验 | 说明 |
|------|------|
| Win32 结构体布局必须对照 SDK 头文件 | Go 结构体的字段对齐规则与 C 不同，不能凭直觉定义 |
| `DialogBoxIndirectParam` 从 goroutine 调用需要 `LockOSThread` | Go 调度器可能移动 goroutine 到不同 OS 线程 |
| `bytes.Buffer` + `binary.Write` 在阻塞 syscall 中可能有 GC 问题 | 改用直接 `append` 的原始字节操作更安全 |
| `MS Shell Dlg` 比具体字体名更可靠 | Windows 自动映射到系统默认 UI 字体 |
| Windows `route add` metric 范围是 1-9999 | 0 不是合法值 |
| 永远要有 fallback | 对话框失败时打开 config.ini 是最可靠的兜底方案 |

## 六、相关文件

| 文件 | 改动 |
|------|------|
| `cmd/client/dialog_windows.go` | 完全重写：leBuffer、LockOSThread、MS Shell Dlg |
| `cmd/client/tray.go` | 对话框失败时 fallback 到 openConfigFile |
| `cmd/client/platform_windows.go` | 新增 openConfigFile |
| `cmd/client/platform_other.go` | 新增 openConfigFile |
| `internal/tun/metric_windows.go` | mibIPInterfaceRow 改用 raw buffer |
| `internal/tun/configure.go` | metric 0 → 1 |
