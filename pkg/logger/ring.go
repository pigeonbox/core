// ring.go 进程内日志环形缓冲（管理端「系统日志」页的真实数据源）。
//
// 历史：admin.GetSystemLogs 曾是返回模拟数据的桩，管理面板展示的是虚构日志；
// 现已修复——环形缓冲实现为 zapcore.Core 并在 Init 时 Tee 进主日志链：零额外
// 调用面（业务代码照常 logger.Info/Warn/Error），容量有界（满载丢最旧），
// 不持久化、重启即清（定位是"最近运行日志"面板，非审计存储——审计在
// admin 域的 operation_logs 表）。
package logger

import (
	"strings"
	"sync"
	"time"

	"go.uber.org/zap/zapcore"
)

// RingEntry 环形缓冲中的一条日志。
type RingEntry struct {
	Seq     uint64    // 单调递增序号（跨绕回不回退，供前端做 ID）
	Time    time.Time // 记录时刻
	Level   string    // debug/info/warn/error/...（zap 小写级别名）
	Message string    // 日志消息正文
	Module  string    // logger 名（zap LoggerName，未命名则为空）
}

// ringCapacity 缓冲条数上限。满载覆盖最旧条目；按平均 200B/条估算内存
// 上界 ~400KB，可忽略。
const ringCapacity = 2000

var logRing = &ringBuffer{}

type ringBuffer struct {
	mu      sync.Mutex
	buf     []RingEntry
	next    uint64 // 写游标（绝对序号）
	wrapped bool   // 是否已绕回（决定读取顺序）
}

func (r *ringBuffer) append(e RingEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	e.Seq = r.next
	if len(r.buf) < ringCapacity {
		r.buf = append(r.buf, e)
		return
	}
	// 满载：覆盖最旧。序号 s 恒落 buf[(s-1) % cap]（与追加段一致），
	// 绕回后 snapshot 的读取起点 next % cap 即最旧存活条目。
	idx := int((r.next - 1) % ringCapacity)
	r.buf[idx] = e
	r.wrapped = true
}

// snapshot 按时间序（最旧在前）返回全部缓冲条目。
func (r *ringBuffer) snapshot() []RingEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.wrapped {
		out := make([]RingEntry, len(r.buf))
		copy(out, r.buf)
		return out
	}
	// 已绕回：写位的下一个即最旧条目
	out := make([]RingEntry, 0, len(r.buf))
	start := int(r.next % ringCapacity)
	for i := 0; i < len(r.buf); i++ {
		out = append(out, r.buf[(start+i)%ringCapacity])
	}
	return out
}

// ringCore zapcore.Core 实现：把日志条目旁路写进环形缓冲。
type ringCore struct {
	level zapcore.LevelEnabler
}

func (c ringCore) Enabled(lvl zapcore.Level) bool { return c.level.Enabled(lvl) }

// With 返回自身：结构化字段不进环形缓冲（面板只展示消息正文，字段随
// encoder 走文件/控制台链路）。
func (c ringCore) With([]zapcore.Field) zapcore.Core { return c }

func (c ringCore) Check(entry zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(entry.Level) {
		return ce.AddCore(entry, c)
	}
	return ce
}

func (c ringCore) Write(entry zapcore.Entry, _ []zapcore.Field) error {
	logRing.append(RingEntry{
		Time:    entry.Time,
		Level:   entry.Level.String(),
		Message: entry.Message,
		Module:  entry.LoggerName,
	})
	return nil
}

func (c ringCore) Sync() error { return nil }

// newRingCore 构造环形缓冲 core（级别与主链一致）。
func newRingCore(level zapcore.LevelEnabler) zapcore.Core {
	return ringCore{level: level}
}

// RecentLogs 查询环形缓冲（管理端系统日志页数据源）。
//   - level: 空 = 全部；否则精确匹配级别名（大小写不敏感，如 "error"）
//   - offset/limit: 分页窗口（按时间序最旧在前；limit ≤ 0 表示不限）
//
// 返回 (窗口内条目, 过滤后总数)。
func RecentLogs(level string, offset, limit int) ([]RingEntry, int) {
	level = strings.ToLower(strings.TrimSpace(level))
	all := logRing.snapshot()
	filtered := all[:0:0]
	for _, e := range all {
		if level == "" || e.Level == level {
			filtered = append(filtered, e)
		}
	}
	total := len(filtered)
	if offset >= total {
		return []RingEntry{}, total
	}
	end := total
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	out := make([]RingEntry, end-offset)
	copy(out, filtered[offset:end])
	return out, total
}
