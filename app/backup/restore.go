package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
}

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
	defer os.RemoveAll(dir)

	manifest, err := s.unpack(r, dir)
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
		src := filepath.Join(dir, filepath.FromSlash(ObjectDirName+entry.Path))
		f, err := os.Open(src)
		if err != nil {
			return nil, fmt.Errorf("打开包内对象失败 %q: %w", entry.Path, err)
		}
		n, err := s.store.SaveStream(ctx, entry.Path, f, entry.Size)
		f.Close()
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
func (s *Service) unpack(r io.Reader, dir string) (*Manifest, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("备份包不是合法的 gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取备份包条目失败: %w", err)
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
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, fmt.Errorf("创建目录失败 %q: %w", name, err)
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("写出条目失败 %q: %w", name, err)
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return nil, fmt.Errorf("写出条目失败 %q: %w", name, err)
		}
		f.Close()
	}

	raw, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return nil, fmt.Errorf("备份包缺少 %s: %w", ManifestName, err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", ManifestName, err)
	}
	return &manifest, nil
}

// verifyObjects 逐对象校验 sha256（包损坏/被截断 ⇒ 立刻拒绝，不落盘）。
func (s *Service) verifyObjects(dir string, manifest *Manifest) error {
	for _, entry := range manifest.Objects {
		src := filepath.Join(dir, filepath.FromSlash(ObjectDirName+entry.Path))
		f, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("备份包缺少对象 %q: %w", entry.Path, err)
		}
		h := sha256Hasher()
		_, err = io.Copy(h, f)
		f.Close()
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
	defer in.Close()

	tmp := dst + ".restore-tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("创建数据库临时文件失败: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("写数据库临时文件失败: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("关闭数据库临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("替换数据库文件失败: %w", err)
	}
	for _, side := range []string{"-wal", "-shm"} {
		_ = os.Remove(dst + side)
	}
	return nil
}
