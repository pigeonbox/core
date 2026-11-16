package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

// sha256Hasher 新建一个 sha256 摘要器。
func sha256Hasher() hash.Hash { return sha256.New() }

// hexOf 取摘要器的 hex 值。
func hexOf(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

// RestoreOptions 恢复选项。
type RestoreOptions struct {
	// Confirm 显式确认（**破坏性操作必须确认**）。
	//
	// 恢复会**替换**数据库文件并重写对象 ⇒ 没确认就恢复 = 把线上数据换成一份
	// 可能不是你要的快照 ⇒ 一律先报错，由调用方（CLI）显式传 --yes。
	Confirm bool
	// DBPath 目标数据库文件（SQLite）。空 = 不恢复 DB（只恢复对象）。
	DBPath string
	// DryRun 只校验（manifest / 摘要 / DB 可用性），不落任何盘。
	DryRun bool
	// Limits 解包资源上限（core#221）；零值 = 默认上限（20 万条目 /
	// 100 GiB / 200:1）。测试可注入小阈值验证拒绝路径。
	Limits UnpackLimits
}

// UnpackLimits 备份解包资源上限（core#221：zip-slip 之后的「写太多」缺口）。
//
// 默认值刻意宽松（正常备份可达几十 GB），只拦「明显不是备份包」的形态：
// 解压炸弹 / 条目洪水 / manifest 申报量级与实际严重不符。
type UnpackLimits struct {
	// MaxEntries 条目数上限（目录/链接/普通文件全部计入）。
	MaxEntries int
	// MaxTotalBytes 解包总量上限（所有常规文件的实际写入字节之和）。
	MaxTotalBytes int64
	// MaxCompressionRatio 压缩比上限（累计输出 / 累计已读压缩输入）。
	// <=0 或非正视为默认；1 表示输出不得大于输入（极严格）。
	MaxCompressionRatio int64
	// CompressionRatioMinBytes 触发压缩比判定的最小累计输出（避免小包误报：
	// gzip 头/小文件天然压缩比很高）。
	CompressionRatioMinBytes int64
}

// 默认解包上限。
const (
	defaultMaxUnpackEntries          = 200_000
	defaultMaxUnpackBytes      int64 = 100 << 30 // 100 GiB
	defaultMaxCompressionRatio int64 = 200
	defaultRatioMinBytes       int64 = 8 << 20 // 8 MiB 才开始判压缩比
)

// DefaultUnpackLimits 默认解包上限（CLI/服务端不传 Limits 时使用）。
func DefaultUnpackLimits() UnpackLimits {
	return UnpackLimits{
		MaxEntries:               defaultMaxUnpackEntries,
		MaxTotalBytes:            defaultMaxUnpackBytes,
		MaxCompressionRatio:      defaultMaxCompressionRatio,
		CompressionRatioMinBytes: defaultRatioMinBytes,
	}
}

// withDefaults 零值字段补默认（部分注入时其余维度仍有保护）。
func (l UnpackLimits) withDefaults() UnpackLimits {
	if l.MaxEntries <= 0 {
		l.MaxEntries = defaultMaxUnpackEntries
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = defaultMaxUnpackBytes
	}
	if l.MaxCompressionRatio <= 0 {
		l.MaxCompressionRatio = defaultMaxCompressionRatio
	}
	if l.CompressionRatioMinBytes <= 0 {
		l.CompressionRatioMinBytes = defaultRatioMinBytes
	}
	return l
}

// unpackLimitError 解包资源上限命中（错误文案面向运维，含实际值与上限）。
type unpackLimitError struct{ msg string }

func (e *unpackLimitError) Error() string { return e.msg }

// RestoreResult 恢复结果。
type RestoreResult struct {
	// Manifest 备份包清单。
	Manifest *Manifest
	// ObjectsRestored / BytesRestored 恢复的对象数与字节数。
	ObjectsRestored int64
	BytesRestored   int64
	// DBRestored 是否已替换数据库。
	DBRestored bool
	// MissingObjects DB 快照引用、但包里没有的对象（导出期间新上传 ⇒ 如实列出，不静默）。
	MissingObjects []string
}

// Restore 从备份包恢复（DB 快照 + 存储对象）。
//
// # 三步，缺一不可
//
//  1. **解包到临时目录**：条目名逐个过 safeEntryName（防 tar 路径穿越）⇒
//     来路不明的包不能往机器上写任意文件。
//  2. **逐项校验**：manifest 的格式版本必须认识；每个对象的 sha256 必须对得上；
//     DB 快照必须能打开。**任一不符 ⇒ 整体中止，一个字节都不落盘**。
//  3. **落盘**：对象经存储层写回（路径逃逸由存储层自防）；DB 用「写临时 + rename」
//     原子替换 ⇒ 中途断电不会留下半个 DB。
func (s *Service) Restore(ctx context.Context, r io.Reader, opts RestoreOptions) (*RestoreResult, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("备份服务未就绪（对象读写未装配）")
	}
	if !opts.Confirm && !opts.DryRun {
		return nil, ErrRestoreNeedsConfirm
	}

	dir, err := os.MkdirTemp("", "pb-restore-*")
	if err != nil {
		return nil, fmt.Errorf("创建恢复临时目录失败: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	manifest, err := s.unpack(r, dir, opts.Limits)
	if err != nil {
		return nil, err
	}
	if manifest.BackupFormat != FormatVersion {
		return nil, fmt.Errorf("%s: %d（本版本只认 %d）", UnsupportedFmt, manifest.BackupFormat, FormatVersion)
	}

	// 校验：逐对象摘要 + DB 快照可用性。
	if err := s.verifyObjects(dir, manifest); err != nil {
		return nil, err
	}
	if err := verifyDBSnapshot(filepath.Join(dir, DBEntryName)); err != nil {
		return nil, err
	}

	result := &RestoreResult{Manifest: manifest}
	result.MissingObjects = s.missingObjects(dir, manifest)

	if opts.DryRun {
		return result, nil
	}

	// 落盘：对象写回当前后端。
	for _, entry := range manifest.Objects {
		// 双点消费（core#166）：落盘前再过一次路径校验——verify 与落盘各自独立
		// 防御，任何一点被绕过都不至于打开解包目录之外的文件。
		clean, err := safeObjectPath(entry.Path)
		if err != nil {
			return nil, fmt.Errorf("manifest 对象路径非法 %q: %w", entry.Path, err)
		}
		src := filepath.Join(dir, filepath.FromSlash(ObjectDirName+clean))
		f, err := os.Open(src)
		if err != nil {
			return nil, fmt.Errorf("打开包内对象失败 %q: %w", entry.Path, err)
		}
		n, err := s.store.SaveStream(ctx, entry.Path, f, entry.Size)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("写回对象失败 %q: %w", entry.Path, err)
		}
		result.ObjectsRestored++
		result.BytesRestored += n
	}

	// 落盘：DB 原子替换。
	if opts.DBPath != "" {
		if err := replaceDB(filepath.Join(dir, DBEntryName), opts.DBPath); err != nil {
			return nil, err
		}
		result.DBRestored = true
	}
	return result, nil
}

// unpack 解包到 dir，返回 manifest。
//
// 资源维度（core#221）：路径防御（safeEntryName/Rel）只保证「不写到别处」，
// 不保证「不写太多」。三个上限在**写入前/写入中**判定：
//   - 条目数：每次 tr.Next() 计数，超限立即中止（条目洪水）；
//   - 解包总量：按实际写入字节累计（不信 hdr.Size），超限立即中止（填盘）；
//   - 压缩比：累计输出 / 累计已读压缩输入，超阈值立即中止（gzip 炸弹）。
//
// manifest 对账：v1 包格式把 manifest 放在**最后**（导出侧需要先算完 sha256），
// 无法「先读 manifest 建预算再解对象」。⇒ 解完后用 objects/ 目录实际大小
// 与 manifest.ObjectBytes 对账（多出 >10%+1MiB ⇒ 视为申报量造假/包被拼接）。
// 单对象级别还有 verifyObjects 的精确大小比对。
func (s *Service) unpack(r io.Reader, dir string, limits UnpackLimits) (*Manifest, error) {
	limits = limits.withDefaults()

	// 计数包装：gzip 读多少压缩字节是压缩比分母的唯一可信来源。
	cr := &countingReader{r: r}
	gz, err := gzip.NewReader(cr)
	if err != nil {
		return nil, fmt.Errorf("备份包不是合法的 gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	entries := 0
	var totalOut int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取备份包条目失败: %w", err)
		}
		entries++
		if entries > limits.MaxEntries {
			return nil, &unpackLimitError{msg: fmt.Sprintf(
				"备份包条目数超过上限（已读 %d > %d）", entries, limits.MaxEntries)}
		}
		name, err := safeEntryName(hdr.Name)
		if err != nil {
			return nil, err
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		// 双保险：解出来的绝对路径必须仍在解包目录内。
		rel, err := filepath.Rel(dir, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("备份包含越界条目: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return nil, fmt.Errorf("创建目录失败 %q: %w", name, err)
			}
			continue
		case tar.TypeReg:
			// ok
		default:
			// 符号链接/设备文件等一律不落盘（备份包不需要它们）。
			continue
		}
		if hdr.Size < 0 {
			return nil, fmt.Errorf("备份包条目 %q 的 size 非法: %d", hdr.Name, hdr.Size)
		}
		// 先按头部声明做一次预算预检（大条目在写第一个字节前就被拦住）。
		if hdr.Size > limits.MaxTotalBytes-totalOut {
			return nil, &unpackLimitError{msg: fmt.Sprintf(
				"备份包解包总量超过上限（声明 %d + 已解 %d > %d）",
				hdr.Size, totalOut, limits.MaxTotalBytes)}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, fmt.Errorf("创建目录失败 %q: %w", name, err)
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("写出条目失败 %q: %w", name, err)
		}
		bw := &budgetWriter{
			w:      f,
			total:  &totalOut,
			cr:     cr,
			limits: limits,
			entry:  name,
		}
		if _, err := io.Copy(bw, tr); err != nil {
			_ = f.Close()
			var le *unpackLimitError
			if errors.As(err, &le) {
				return nil, le
			}
			return nil, fmt.Errorf("写出条目失败 %q: %w", name, err)
		}
		_ = f.Close()
	}

	raw, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return nil, fmt.Errorf("备份包缺少 %s: %w", ManifestName, err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", ManifestName, err)
	}

	// manifest 申报量对账：objects/ 实际字节数不得显著超过 ObjectBytes。
	if manifest.ObjectBytes > 0 {
		actual, err := dirRegularBytes(filepath.Join(dir, filepath.FromSlash(strings.TrimSuffix(ObjectDirName, "/"))))
		if err != nil {
			return nil, fmt.Errorf("统计包内对象字节数失败: %w", err)
		}
		slack := int64(1 << 20)
		if extra := manifest.ObjectBytes / 10; extra > slack {
			slack = extra
		}
		if actual > manifest.ObjectBytes+slack {
			return nil, fmt.Errorf(
				"备份包对象字节数超过 manifest 申报（实际 %d > 申报 %d + 容差 %d）⇒ 包内容与清单不符",
				actual, manifest.ObjectBytes, slack)
		}
	}
	return &manifest, nil
}

// countingReader 统计从底层读取的压缩字节数（压缩比分母）。
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// budgetWriter 边写边判解包总量与压缩比（core#221）。
//
// 判定发生在**写入前**：超限时本次 Write 一个字节都不落，磁盘占用以已写量为界
// （≤ MaxTotalBytes），不会等整条/整个包写完才发现。
type budgetWriter struct {
	w      io.Writer
	total  *int64
	cr     *countingReader
	limits UnpackLimits
	entry  string
}

func (b *budgetWriter) Write(p []byte) (int, error) {
	n := int64(len(p))
	next := *b.total + n
	if next > b.limits.MaxTotalBytes {
		return 0, &unpackLimitError{msg: fmt.Sprintf(
			"备份包解包总量超过上限（%d > %d，条目 %q）",
			next, b.limits.MaxTotalBytes, b.entry)}
	}
	// 压缩比：累计输出达到最小判定量后，输出/已读压缩输入 > 上限即中止。
	if b.cr != nil && next >= b.limits.CompressionRatioMinBytes && b.cr.n > 0 {
		if next/b.cr.n > b.limits.MaxCompressionRatio {
			return 0, &unpackLimitError{msg: fmt.Sprintf(
				"备份包压缩比超过上限（%d:1 > %d:1，已解 %d 字节 / 已读压缩 %d 字节）",
				next/b.cr.n, b.limits.MaxCompressionRatio, next, b.cr.n)}
		}
	}
	written, err := b.w.Write(p)
	*b.total += int64(written)
	return written, err
}

// dirRegularBytes 统计目录下常规文件字节数（目录不存在 = 0）。
func dirRegularBytes(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info != nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// verifyObjects 逐对象校验 sha256（包损坏/被截断 ⇒ 立刻拒绝，不落盘）。
//
// 路径校验先于 os.Open（core#166）：manifest 对象路径逃出解包目录 ⇒ 拒绝，
// 不能拿宿主机上的任意文件当「摘要比对源」（sha256 口 oracle）或让 FIFO 挂死。
// verify 在 DryRun 返回之前执行 ⇒ DryRun 同样中招不了。
func (s *Service) verifyObjects(dir string, manifest *Manifest) error {
	for _, entry := range manifest.Objects {
		clean, err := safeObjectPath(entry.Path)
		if err != nil {
			return fmt.Errorf("manifest 对象路径非法 %q: %w", entry.Path, err)
		}
		src := filepath.Join(dir, filepath.FromSlash(ObjectDirName+clean))
		// 精确大小比对（core#221）：sha256 只读 entry.Size 字节 ⇒ 若文件尾部
		// 被追加内容，摘要仍然匹配。先按磁盘真实大小与 manifest 声明比对。
		st, err := os.Stat(src)
		if err != nil {
			return fmt.Errorf("备份包缺少对象 %q: %w", entry.Path, err)
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("备份包对象 %q 不是常规文件", entry.Path)
		}
		if st.Size() != entry.Size {
			return fmt.Errorf("对象 %q 大小与 manifest 不符（磁盘 %d 声明 %d）",
				entry.Path, st.Size(), entry.Size)
		}
		f, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("备份包缺少对象 %q: %w", entry.Path, err)
		}
		h := sha256Hasher()
		// LimitReader 兜底：Size>0 时不在声明字节之外读（超大/FIFO 内容不挂死）。
		// Size==0 是**合法空对象**（写方 Size=实际字节数，见 export.writeObject）
		// ⇒ 此时须全量读：空读的摘要恒为 sha256("")，会给「Size=0 但实际非空」
		// 的包放行；路径已校验在解包目录内、且 unpack 只落常规文件 ⇒ 全量读安全。
		var r io.Reader = f
		if entry.Size > 0 {
			r = io.LimitReader(f, entry.Size)
		}
		_, err = io.Copy(h, r)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("读取包内对象失败 %q: %w", entry.Path, err)
		}
		if got := hexOf(h); got != entry.SHA256 {
			return fmt.Errorf("对象 %q 摘要不符（期望 %s 实得 %s）⇒ 备份包已损坏", entry.Path, entry.SHA256, got)
		}
	}
	return nil
}

// verifyDBSnapshot 校验 DB 快照可打开且含核心表（坏包不许落盘）。
func verifyDBSnapshot(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("备份包缺少数据库快照 %s: %w", DBEntryName, err)
	}
	if st.Size() == 0 {
		return fmt.Errorf("数据库快照为空")
	}
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("数据库快照无法打开: %w", err)
	}
	defer closeDB(db)
	var n int64
	if err := db.Raw("SELECT count(*) FROM file_codes").Scan(&n).Error; err != nil {
		return fmt.Errorf("数据库快照缺少分享表（file_codes）: %w", err)
	}
	return nil
}

// missingObjects 列出「DB 快照引用、但包里没有」的对象（导出期间新上传 ⇒ 如实报）。
func (s *Service) missingObjects(dir string, manifest *Manifest) []string {
	have := make(map[string]bool, len(manifest.Objects))
	for _, o := range manifest.Objects {
		have[o.Path] = true
	}
	db, err := gorm.Open(sqlite.Open(filepath.Join(dir, DBEntryName)), &gorm.Config{})
	if err != nil {
		return nil
	}
	defer closeDB(db)
	var rows []model.FileCode
	if err := db.Order("id asc").Find(&rows).Error; err != nil {
		return nil
	}
	var missing []string
	for _, r := range rows {
		p := strings.TrimSpace(r.FilePath)
		if p == "" || have[p] {
			continue
		}
		missing = append(missing, p)
	}
	return missing
}

// closeDB 关闭 gorm 打开的 SQLite 句柄（gorm 自身没有 Close）。
func closeDB(db *gorm.DB) {
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// replaceDB 原子替换数据库文件（写临时 + rename）。
//
// 同时清掉 WAL/SHM 边车：旧库的 WAL 里可能有未落盘的页 ⇒ 换库后残留会污染新库。
func replaceDB(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开数据库快照失败: %w", err)
	}
	defer func() { _ = in.Close() }()

	tmp := dst + ".restore-tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("创建数据库临时文件失败: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("写数据库临时文件失败: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("关闭数据库临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换数据库文件失败: %w", err)
	}
	for _, side := range []string{"-wal", "-shm"} {
		_ = os.Remove(dst + side)
	}
	return nil
}
