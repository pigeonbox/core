package backup

// 定时备份纯逻辑的单测：到期判定 / 命名 / 保留策略。
//
// 判据都带**负对照**：不只验「该跑的时候跑」，也验「不该跑的时候不跑」、
// 「该删的时候删」与「不该删的时候一份都不动」。

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseDailyAt 时刻解析：合法写法规范化，非法写法一律报错（不猜）。
func TestParseDailyAt(t *testing.T) {
	ok := map[string]DailyAt{
		"03:00":   {Hour: 3, Minute: 0},
		"3:05":    {Hour: 3, Minute: 5},
		" 23:59 ": {Hour: 23, Minute: 59},
		"00:00":   {Hour: 0, Minute: 0},
		"":        {Hour: 3, Minute: 0}, // 空串 = 默认 03:00
	}
	for in, want := range ok {
		got, err := ParseDailyAt(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"24:00", "03:60", "-1:00", "0300", "03-00", "ab:cd", "03:00:00"} {
		_, err := ParseDailyAt(bad)
		assert.Error(t, err, "应当拒绝非法时刻: %q", bad)
	}
}

// TestDailyAt_Due 到期判定：**时刻已过 且 当天无包** 才跑。
func TestDailyAt_Due(t *testing.T) {
	at := DailyAt{Hour: 3, Minute: 0}
	day := func(h, m int) time.Time { return time.Date(2026, 11, 6, h, m, 0, 0, time.Local) }

	// 主判据：过了时刻、当天没包 ⇒ 跑。
	assert.True(t, at.Due(day(3, 0), false), "恰好到点就该跑")
	assert.True(t, at.Due(day(9, 30), false), "过点未备份 ⇒ 必须补跑（重启/catch-up）")
	// 负对照 1：还没到点 ⇒ 不跑（否则凌晨配置形同虚设）。
	assert.False(t, at.Due(day(2, 59), false))
	assert.False(t, at.Due(day(0, 5), false))
	// 负对照 2：当天已有包 ⇒ 不跑（否则每轮循环都重新导出）。
	assert.False(t, at.Due(day(9, 30), true))
	assert.False(t, at.Due(day(23, 59), true))
}

// TestFileName_RoundTrip 命名与解析互逆，且 `.partial` 不算成型包。
func TestFileName_RoundTrip(t *testing.T) {
	ts := time.Date(2026, 11, 6, 3, 4, 5, 0, time.Local)
	name := FileName(ts)
	assert.Equal(t, "pigeonbox-backup-20261106-030405.tar.gz", name)
	got, ok := ParseFileName(name)
	require.True(t, ok)
	// 往返必须回到同一**墙上时间**（时区也一致）：用 Parse 会拿到 UTC，
	// 在 UTC+8 上差 8 小时（见 ParseFileName 的注释）。
	assert.Equal(t, ts.Unix(), got.Unix())
	assert.Equal(t, ts.Format(AutoFileLayout), got.Format(AutoFileLayout))
	assert.Equal(t, "20261106", DateKey(ts))
	key, ok := DateKeyOfFileName(name)
	require.True(t, ok)
	assert.Equal(t, "20261106", key)

	// 时区陷阱的负对照：本地 00:30 的包，日期键必须还是**当天**
	// （若走 Parse+UTC 会变成前一天 ⇒ 幂等失效 ⇒ 每轮重复备份）。
	early := time.Date(2026, 11, 6, 0, 30, 0, 0, time.Local)
	earlyName := FileName(early)
	assert.True(t, HasBackupForDate([]FileInfo{{Name: earlyName}}, DateKey(early)),
		"本地早于 UTC 偏移量的时刻，日期键仍必须落在当天")

	// 负对照：临时文件、别家的包、名字畸形都不算。
	for _, bad := range []string{
		name + PartialSuffix,
		"manifest.json",
		"pigeonbox-backup-x.tar.gz",
		"backup-20261106-030405.tar.gz",
		"pigeonbox-backup-20261106-030405.zip",
		"",
	} {
		_, ok := ParseFileName(bad)
		assert.False(t, ok, "不该被认成备份包: %q", bad)
	}
}

// TestSortNewestFirst 排序按**名字里的时间**而非 mtime（拷贝会改写 mtime）。
func TestSortNewestFirst(t *testing.T) {
	mk := func(ts time.Time) FileInfo {
		return FileInfo{Name: FileName(ts), ModTime: time.Unix(0, 0)} // mtime 全是 0
	}
	old := mk(time.Date(2026, 11, 1, 3, 0, 0, 0, time.Local))
	mid := mk(time.Date(2026, 11, 3, 3, 0, 0, 0, time.Local))
	new := mk(time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local))
	files := []FileInfo{old, new, mid}
	SortNewestFirst(files)
	assert.Equal(t, []string{new.Name, mid.Name, old.Name}, []string{files[0].Name, files[1].Name, files[2].Name})
}

// TestSelectForRetention 保留策略：留最新 N 份；`keep<=0` **一份都不删**。
func TestSelectForRetention(t *testing.T) {
	mkDay := func(day int) FileInfo {
		return FileInfo{Name: FileName(time.Date(2026, 11, day, 3, 0, 0, 0, time.Local))}
	}
	files := []FileInfo{mkDay(1), mkDay(2), mkDay(3), mkDay(4), mkDay(5)}

	deleted := SelectForRetention(files, 3)
	require.Len(t, deleted, 2)
	assert.Equal(t, mkDay(2).Name, deleted[0].Name)
	assert.Equal(t, mkDay(1).Name, deleted[1].Name)
	// 最新的那份**永远**不在待删集合里。
	for _, f := range deleted {
		assert.NotEqual(t, mkDay(5).Name, f.Name, "绝不删最新的包")
	}

	// 负对照 1：keep<=0（含误配成 0 / 取到零值）= 不限 ⇒ 一份都不删。
	assert.Empty(t, SelectForRetention(files, 0))
	assert.Empty(t, SelectForRetention(files, -1))
	// 负对照 2：份数不足 ⇒ 不删。
	assert.Empty(t, SelectForRetention(files, 5))
	assert.Empty(t, SelectForRetention(files, 99))
	// 负对照 3：空目录不 panic。
	assert.Empty(t, SelectForRetention(nil, 3))
}

// TestListFiles 列目录：只认成型包（临时文件与杂物被排除），目录缺失不算错误。
func TestListFiles(t *testing.T) {
	dir := t.TempDir()

	// 目录缺失（还没备份过）⇒ 空列表 + 无错误。
	missing := filepath.Join(dir, "nope")
	files, err := ListFiles(missing)
	require.NoError(t, err)
	assert.Empty(t, files)

	ts := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	good := filepath.Join(dir, FileName(ts))
	require.NoError(t, os.WriteFile(good, []byte("x"), 0o600))
	// 半个包（导出中被杀）与杂物都必须被排除。
	require.NoError(t, os.WriteFile(good+PartialSuffix, []byte("half"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "subdir"), 0o700))

	files, err = ListFiles(dir)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, FileName(ts), files[0].Name)
	assert.Equal(t, int64(1), files[0].Size)
}

// TestHasBackupForDate 幂等判定的唯一依据：目录里有没有当天日期的包。
func TestHasBackupForDate(t *testing.T) {
	ts := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	files := []FileInfo{{Name: FileName(ts)}}
	assert.True(t, HasBackupForDate(files, "20261106"))
	assert.False(t, HasBackupForDate(files, "20261107"))
	assert.False(t, HasBackupForDate(nil, "20261106"))
}
