# GameTunnel 会话记录 — 2026-05-11

## 一、架构讨论：服务端与客户端能否合并

### 结论：技术上可以，但不建议

当前架构是 C/S 模型：客户端通过公网 VPS 中转游戏数据包。

合并（去掉独立服务端，某个客户端兼任 host）的核心障碍：

| 障碍 | 说明 |
|------|------|
| NAT 问题 | 家庭网络无法被外部主动连入，总需要一台公网可达的机器 |
| IP 分配权威性 | 服务端负责分配虚拟 IP，合并后谁来当权威？ |
| 稳定性 | host 玩家退出则整个房间崩溃，需要复杂的 Host Migration |

**路由不混乱**：服务端的 `handleRelay` 逻辑只有三条规则（广播→转发全部、组播→转发全部、单播→转发目标），通过 `clients` map 隐式查表，设计清晰。

---

## 二、Android 手机作为服务器

### 结论：网络不可行

| 网络环境 | 能否作为服务器 |
|---------|-------------|
| 4G/5G 蜂窝 | ❌ 运营商 CGNAT，无公网 IP |
| 家庭 WiFi | ⚠️ 需路由器端口转发，不如直接在路由器跑 |
| 公网 WiFi | ❌ 防火墙不允许 |

瓶颈不是性能，是网络环境。

---

## 三、OpenWrt 路由器作为服务器（✅ 已实现）

### 为什么合适

- 路由器本身就是网络出口，UDP 4700 对外天然可达
- 7×24 运行，零额外成本
- 服务端代码极轻量，ARM 路由器 CPU/内存毫无压力

### 推荐设备

- NanoPi R2S/R4S/R5S（ARM64，百元价位）
- 树莓派 4/5
- GL.iNet 系列
- 不推荐 MIPS 架构低端路由器

### 已完成的代码改动

#### Makefile 新增目标

| Target | 说明 |
|--------|------|
| `server-openwrt-arm64` | OpenWrt ARM64 服务端 |
| `server-openwrt-armv7` | OpenWrt ARMv7 服务端 |
| `server-openwrt` | 所有 OpenWrt 架构 |
| `release-openwrt` | 打包 OpenWrt 发布文件 |

#### 新增 `scripts/install-openwrt.sh`

与 `install-server.sh`（systemd）的区别：

| 特性 | install-server.sh | install-openwrt.sh |
|------|-------------------|---------------------|
| 初始化系统 | systemd | procd（OpenWrt 标准） |
| 防火墙 | 手动配置 | UCI 自动配置 |
| 配置文件 | 命令行参数 | `/etc/gtunnel-server.conf` |
| 日志 | journalctl | logread |
| 内存检测 | 无 | 低内存警告 |

用户使用流程：
```bash
# 一行命令
wget -qO- https://raw.githubusercontent.com/holipay/gametunnel/main/scripts/install-openwrt.sh | sh

# 带密码
ROOM_PASSWORD=mypass wget -qO- ... | sh
```

---

## 四、平台增删（已实现）

### 删除的平台

| 平台 | 原因 |
|------|------|
| `server-linux-arm64` | OpenWrt ARM64 已覆盖，独立 Linux ARM64 很少用 |
| `server-windows-arm64` | Windows ARM64 服务器几乎不存在 |
| `client-windows-arm64` | 同上 |

### 新增的平台

| 平台 | 原因 |
|------|------|
| `server-windows-x86` | 32 位 Windows 服务端备用 |
| `client-windows-x86` | **老游戏 + 老电脑的主力环境**（WinXP/Win7 32 位） |

### 最终 Release 产物

```
GameTunnel-Client-windows-amd64.zip     # 64 位客户端
GameTunnel-Client-windows-x86.zip       # 32 位客户端（新增）
GameTunnel-Server-linux-amd64.tar.gz    # Linux 服务端
GameTunnel-Server-windows-amd64.zip     # Win64 服务端
GameTunnel-Server-windows-x86.zip       # Win32 服务端（新增）
GameTunnel-Server-openwrt-arm64.tar.gz  # OpenWrt ARM64（新增）
GameTunnel-Server-openwrt-armv7.tar.gz  # OpenWrt ARMv7（新增）
```

---

## 五、i18n 多语言审查（已修复）

### 架构

单文件 `internal/i18n/i18n.go`，结构体 `Strings` 包含 120 个字段，中英文各一套。

### 审查结果

| 维度 | 状态 |
|------|------|
| 中英字段数一致性 | ✅ 120 = 120 |
| 格式化占位符一致性 | ✅ 全部匹配 |
| 服务端代码覆盖 | ✅ 完整 |
| 客户端代码覆盖 | ⚠️ 有遗漏（已修复） |

### 已修复的问题

| 问题 | 修复 |
|------|------|
| `SaveConfig` 硬编码英文注释 | 改用 `i18n.T().CfgXxx` |
| UAC 提权错误硬编码英文 | 新增 `ErrElevateFailed` 字段 |
| `parseHostIP` 的 `net.SplitHostPort` 返回值不匹配 | `host, err` → `host, _, err` |

### 可接受的硬编码（不改）

| 位置 | 原因 |
|------|------|
| flag 描述（`-addr`, `-subnet` 等） | CLI 惯例，`-help` 输出英文是标准做法 |
| `tun/` 包内部日志 | 开发者调试日志，普通用户不看 |
| `singleinstance` 错误信息 | 外层已 i18n 包装 |

---

## 六、用户体验审查

### 目标用户画像

不懂电脑的中老年局域网游戏玩家。需要极简的安装和设置流程。

### 用户旅程

```
下载解压 → 双击exe → UAC弹窗 → 托盘图标出现 → 点"连接" → 提示填写服务器 → 填完确定 → 连接成功/失败
```

### 🔴 P0 关键问题

| 问题 | 现状 | 建议 |
|------|------|------|
| 连接失败无错误提示 | 托盘只显示"🔴 未连接" | 显示具体错误或弹窗 |
| 首次运行无引导 | 用户不知道下一步干嘛 | 自动弹出设置对话框 |

### 🟡 P1 中等问题

| 问题 | 现状 | 建议 |
|------|------|------|
| 输入无验证 | 填 `abc` 也能点确定 | 服务器地址格式校验 |
| 自动重连太安静 | 服务器地址错了会静默重试 60 秒 | 前 3 次失败后弹窗 |
| 密码无确认 | 输错不知道 | 添加"显示密码"复选框 |
| 托盘图标被隐藏 | Win10/11 默认藏新图标 | 首次运行弹气泡通知 |

### 🟢 P2 小优化

| 问题 | 建议 |
|------|------|
| config.ini 英文注释 | 改为中文 |
| 缺少"编辑配置"菜单 | 托盘加"📝 编辑配置文件" |
| 设置对话框按钮 | 首次设置用"连接"代替"确定" |
| 语言切换不即时 | 加提示"重启后生效" |

---

## 七、Git 提交记录

| Commit | 内容 |
|--------|------|
| `2c19119` | feat: add OpenWrt server platform, add Windows x86, remove arm64 targets |
| `0fd5165` | fix: net.SplitHostPort return value mismatch, i18n fixes |
