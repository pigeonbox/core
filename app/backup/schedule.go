package backup

// 定时备份的**纯逻辑**：时刻解析 / 到期判定 / 命名 / 保留策略。
//
// 这里刻意不碰 *Service、不碰全局配置、不读钟（now 一律由调用方传入）：
// 调度器（scheduler.go）与 HTTP 管理面共用同一套判据，而这些判据本身可以
// 逐条单测（含负对照——例如「当天已有包就不再跑」「keep<=0 一份都不删」）。
//
// # 为什么幂等靠文件名
//
// 「今天备份过没有」最容易想到的做法是**另存一个状态文件**（last_run.json）。
// 但状态文件会与备份目录**分叉**：人手工删了包、或者从别的机器拷了包过来，
// 状态文件仍然说「今天跑过了」⇒ 这一天就没有备份，而且没有任何迹象。
// ⇒ 让备份**目录本身**是唯一真相：文件名里带日期，列出目录就知道有没有。
// 代价是备份必须落在本目录（不支持挪走）——这个代价换来「删包=下次会补」的
// 自洽语义，值得。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 自动备份包的文件名约定（解析与列出都依赖它，**改动即破坏幂等**）。
const (
	// AutoFilePrefix 自动备份包名前缀。
	AutoFilePrefix = "pigeonbox-backup-"
	// AutoFileLayout 文件名里的时间戳布局（日期在前 ⇒ 按名字排序即按时间排序）。
	AutoFileLayout = "20060102-150405"
	// AutoFileExt 包后缀（与离线子命令导出的一致，便于互相恢复）。
	AutoFileExt = ".tar.gz"
	// PartialSuffix 导出中的临时后缀：**先写临时文件、成功后改名**。
	//
	// 为什么必须这样：导出要写几十 GB，中途磁盘满/被杀会留下半个包。若直接
	// 写最终名，下次列目录会把它当成「今天已备份」⇒ 当天真正的一次备份被
	// 跳过，且用户以为备份正常。临时后缀同时把「半个包」从保留策略里排除。
	PartialSuffix = ".partial"
	// DateKeyLayout 日期键布局（幂等判定的依据）。
	DateKeyLayout = "20060102"
	// DefaultDailyAt 默认每日时刻（凌晨，避开取件高峰）。
	DefaultDailyAt = "03:00"
)

// DailyAt 每天的目标时刻（本地时区）。
type DailyAt struct {
	Hour   int
	Minute int
}

// ParseDailyAt 解析 `HH:MM`（空串取默认 03:00）。
//
// 只接受 `H:MM` / `HH:MM`：连字符、空格分隔、「3点」这类写法一律报错，
// 而不是猜——备份时刻猜错一小时不会有人发现，直到需要那个包。
func ParseDailyAt(s string) (DailyAt, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		t = DefaultDailyAt
	}
	parts := strings.SplitN(t, ":", 2)
	if len(parts) != 2 {
		return DailyAt{}, fmt.Errorf("备份时刻格式非法（应为 HH:MM）: %q", s)
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return DailyAt{}, fmt.Errorf("备份时刻的小时非法（应为 HH:MM）: %q", s)
	}
	m, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return DailyAt{}, fmt.Errorf("备份时刻的分钟非法（应为 HH:MM）: %q", s)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return DailyAt{}, fmt.Errorf("备份时刻超出范围（00:00–23:59）: %q", s)
	}
	return DailyAt{Hour: h, Minute: m}, nil
}

// String 规范化成 HH:MM。
func (d DailyAt) String() string {
	return fmt.Sprintf("%02d:%02d", d.Hour, d.Minute)
}

// SlotFor 返回 now 所在自然日里的目标时刻（同一时区）。
func (d DailyAt) SlotFor(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day(), d.Hour, d.Minute, 0, 0, now.Location())
}

// Due 是否该补跑：**目标时刻已过** 且 **当天还没有包**。
//
// 两个条件缺一不可：
//   - 少了「已过时刻」⇒ 一启动就备份，凌晨配置形同虚设；
//   - 少了「当天无包」⇒ 每轮循环都重跑（把所有存储读一遍）。
//
// ⚠️ 机器连停三天后启动，只在当天补一份（不补中间两天）——历史时点的快照
// 补不出来（数据已经是「现在」的），假装能补才是错的。
func (d DailyAt) Due(now time.Time, hasBackupToday bool) bool {
	if hasBackupToday {
		return false
	}
	return !now.Before(d.SlotFor(now))
}

// FileName 生成某个时刻的备份包名。
func FileName(t time.Time) string {
	return AutoFilePrefix + t.Format(AutoFileLayout) + AutoFileExt
}

// DateKey 时刻的日期键（YYYYMMDD）。
func DateKey(t time.Time) string {
	return t.Format(DateKeyLayout)
}

// ParseFileName 解析备份包名，返回文件名里的时间（不是 mtime！）。
//
// 用**名字里的时间**而非文件 mtime：拷贝/恢复会改写 mtime，但名字里的时间
// 是不变的事实。
//
// ⚠️ 必须用 ParseInLocation(time.Local) 而不是 time.Parse：后者给出 UTC 时区，
// 而文件名是按**本地时区**格式化出来的 ⇒ 在 UTC+8 上，凌晨 03:00 的包解析成
// UTC 前一天 19:00 ⇒ 日期键推前一天 ⇒ 「今天已备份」判成「今天没备份」⇒
// 每轮循环都重新导出（10 分钟一次，把存储读穿）。
func ParseFileName(name string) (time.Time, bool) {
	stamp, ok := fileNameStamp(name)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(AutoFileLayout, stamp, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// fileNameStamp 取文件名里的时间戳串（格式校验也在这一步）。
func fileNameStamp(name string) (string, bool) {
	base := filepath.Base(name)
	if !strings.HasPrefix(base, AutoFilePrefix) || !strings.HasSuffix(base, AutoFileExt) {
		return "", false
	}
	stamp := strings.TrimSuffix(strings.TrimPrefix(base, AutoFilePrefix), AutoFileExt)
	if len(stamp) != len(AutoFileLayout) {
		return "", false
	}
	return stamp, true
}

// DateKeyOfFileName 从文件名取日期键（`YYYYMMDD`），**直接取字面量**。
//
// 为什么不绕道 ParseFileName 再 Format：日期键的语义是「文件名里写的哪一天」
// （按生成时的本地时区），任何时区换算都可能把它挪到前一天 ⇒ 幂等失效。
// 字面量取前 8 位没有这个风险，且与 DateKey(now) 的口径天然一致。
func DateKeyOfFileName(name string) (string, bool) {
	stamp, ok := fileNameStamp(name)
	if !ok {
		return "", false
	}
	return stamp[:len(DateKeyLayout)], true
}

// IsBackupFileName 是否是**成型**的备份包（临时文件不算）。
func IsBackupFileName(name string) bool {
	_, ok := ParseFileName(name)
	return ok
}

// FileInfo 备份目录里的一份包（列目录的结果）。
type FileInfo struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// SortNewestFirst 按文件名里的时间倒序（新的在前）。
//
// 排序键是**解析出的时间**，不是原始字符串：两者在合法命名下等价，但解析
// 失败的名字会被排到最后，避免混进来的怪文件名影响保留策略的选择。
func SortNewestFirst(files []FileInfo) {
	sort.SliceStable(files, func(i, j int) bool {
		ti, oki := ParseFileName(files[i].Name)
		tj, okj := ParseFileName(files[j].Name)
		if oki != okj {
			return oki
		}
		return ti.After(tj)
	})
}

// ListFiles 列出目录里的成型备份包（不含 `.partial`，按新→旧排序）。
//
// 目录不存在**不是错误**（还没备份过）⇒ 返回空列表。
func ListFiles(dir string) ([]FileInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取备份目录失败: %w", err)
	}
	files := make([]FileInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !IsBackupFileName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			// 并发删除（人手工清理）⇒ 跳过即可，不该让整个列表失败。
			continue
		}
		files = append(files, FileInfo{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime()})
	}
	SortNewestFirst(files)
	return files, nil
}

// HasBackupForDate 目录里是否已有该日期（YYYYMMDD）的包。
func HasBackupForDate(files []FileInfo, date string) bool {
	for _, f := range files {
		if key, ok := DateKeyOfFileName(f.Name); ok && key == date {
			return true
		}
	}
	return false
}

// SelectForRetention 选出该删的包：保留最新 keep 份，其余删掉。
//
// `keep <= 0` ⇒ **一份都不删**（不限）。
//
// 为什么把 0 解释成「不限」而不是「全删」：这是安全方向的唯一选择。保留份数
// 是运维拍脑袋填的数字，填 0 / 留空 / 配置项改名后取到零值都有可能；把它当
// 「删光」意味着一次配置错误就清空全部备份——而备份是**最后一道退路**。
// 同理，切到「保留最近 N 份」时**最新的那份永远不在待删集合里**。
func SelectForRetention(files []FileInfo, keep int) []FileInfo {
	if keep <= 0 || len(files) <= keep {
		return nil
	}
	sorted := make([]FileInfo, len(files))
	copy(sorted, files)
	SortNewestFirst(sorted)
	return sorted[keep:]
}
