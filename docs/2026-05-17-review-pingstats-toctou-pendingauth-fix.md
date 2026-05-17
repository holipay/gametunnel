# GameTunnel Code Review & Bugfix Report
## ringbuf · toctou · auth-underflow · timer-leak · ip-validation · atomic-cipher

---

## 修复总览

| 优先级 | 类别 | 数量 |
|--------|------|------|
| 🔴 确认 Bug | 缺陷 | 6 |
| 🟡 潜在风险 | 竞态/泄漏 | 6 |
| 🟢 优化建议 | 性能/可维护性 | 6 |

**涉及文件**: 7 files, +54 -20

---

## 🔴 确认 Bug

### 1. PingStats 环形缓冲区遍历错误

**文件**: `internal/server/server.go:62-68`

```go
func (c *Client) PingStats() (lossRate float64, jitter time.Duration) {
    total := c.pingIdx
    if total == 0 {
        return 0, 0
    }
    n := total
    if n > pingHistorySize {
        n = pingHistorySize
    }
    for i := 0; i < n; i++ {
        rtt := c.pingHistory[i]  // ← 错误：从 0 开始遍历
