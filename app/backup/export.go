package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/oekit/core/version"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

// ObjectLister 列出被 DB 引用的对象路径（admin 域实现）。
//
// **本地声明窄接口**（不 import app/storage）：Go 的结构化接口让 admin 的服务
// 直接满足它 ⇒ 备份域与 app/storage **零耦合**（域间跨域要登记白名单，棘轮守卫
// 会拦）⇒ 同时与存储迁移口径完全一致：两处都含**软删**记录
// （软删分享可恢复 ⇒ 对象也得带走）。
type ObjectLister interface {
	// ListReferencedObjectPaths 返回全部被引用的对象路径（去重）。
	ListReferencedObjectPaths(ctx context.Context) ([]string, error)
}

// ObjectStore 读写存储对象（core/storage 单例实现）。
//
// 用**接口**而非具体类型：备份域只依赖「读一个对象 / 写一个对象」两件事，
// 不必知道后端是 local 还是 S3 ⇒ 恢复时对象**原样写回当前后端**（换后端 + 换机器
// 一次做完）。
type ObjectStore interface {
	// GetFileReader 打开对象读取器（同时给出大小）。
	GetFileReader(ctx context.Context, filePath string) (io.ReadCloser, int64, error)
	// EffectiveType 当前生效的存储后端类型（记进 manifest ⇒ 恢复时知道源后端）。
	EffectiveType() string
	// SaveStream 把 reader 写入相对路径（路径逃逸由存储层自防）。
	SaveStream(ctx context.Context, savePath string, r io.Reader, expectedSize int64) (int64, error)
	// FileExists 对象是否已存在（**存量数据缺失**的判定：读之前先问一句，
	// 免得把「文件本来就没了」与「存储后端出故障」混为一谈）。
	FileExists(ctx context.Context, filePath string) bool
}

// Service 备份包服务。
type Service struct {
	db      *gorm.DB
	objects ObjectLister
	store   ObjectStore
	// dbType 数据库类型（默认取自方言；单测可改 ⇒ 覆盖「非 SQLite 明确报错」分支）。
	dbType string
}

// NewService 创建备份包服务。
//
// 依赖都是**窄接口**（对象清单 / 对象读写 / DB）⇒ 备份域不直接依赖任何业务域，
// 装配在 bootstrap 完成（与「存储域不直接碰 DB」同款纪律）。
func NewService(db *gorm.DB, objects ObjectLister, store ObjectStore) *Service {
	return &Service{db: db, objects: objects, store: store, dbType: db.Name()}
}

// Export 把「DB 快照 + 文件清单 + 存储对象 + 校验」导出成一个 tar.gz 包。
//
// # 顺序（为什么 manifest 最后写）
//
// manifest 要含**每个对象的 sha256** ⇒ 只有对象都写完才知道 ⇒ manifest 放最后。
// （代价：包内 manifest 不在头部；换来的是**只读一遍对象**，大实例不用双倍 IO。）
//
// # 一致性口径
//
// DB 快照（VACUUM INTO）是一致视图；对象是**逐个读**的 ⇒ 导出期间新上传的对象
// 可能只进包不进快照（或反之）⇒ 恢复后这类记录会指向缺失对象 ⇒ 恢复结果里
// **如实列出缺失清单**（Missing），不静默。
func (s *Service) Export(ctx context.Context, w io.Writer) (*Manifest, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("备份服务未就绪（DB 未装配）")
	}
	if s.objects == nil || s.store == nil {
		return nil, fmt.Errorf("备份服务未就绪（对象清单/对象读写未装配）")
	}

	paths, err := s.objects.ListReferencedObjectPaths(ctx)
	if err != nil {
		return nil, fmt.Errorf("列举待备份对象失败: %w", err)
	}
	// 稳定排序 ⇒ 同样的实例导出得到同样的条目顺序 ⇒ 便于逐包比对。
	sort.Strings(paths)

	manifest := &Manifest{
		BackupFormat: FormatVersion,
		CreatedAt:    time.Now().Format(time.RFC3339),
		AppVersion:   version.Version,
		CoreVersion:  version.Version,
		DBType:       s.dbType,
		StorageType:  s.store.EffectiveType(),
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	// 1) 对象：边写边算 sha256（只读一遍）。
	for _, p := range paths {
		// 存量缺失（DB 有引用、磁盘上已没有）⇒ 跳过并**如实记账**。
		//
		// 为什么不能整体失败：真机实录（2026-10-31，10.10.10.3）里这台实例的 DB
		// 有 300+ 条分享记录，其中若干对象文件早被清理 ⇒ 「读不到就整体失败」⇒
		// **备份包在这台实例上根本导不出来** ⇒ 灾备能力在最需要它的机器上失效。
		// 而「存储后端真出故障」仍会报错（下面的读失败分支）⇒ 不吞真故障。
		if !s.store.FileExists(ctx, p) {
			manifest.MissingObjects = append(manifest.MissingObjects, p)
			continue
		}
		entry, err := s.writeObject(ctx, tw, p)
		if err != nil {
			return nil, err
		}
		manifest.Objects = append(manifest.Objects, entry)
		manifest.ObjectCount++
		manifest.ObjectBytes += entry.Size
	}

	// 2) DB 快照。
	if err := s.writeDBSnapshot(ctx, tw, manifest); err != nil {
		return nil, err
	}

	// 3) 文件清单（链接不变的独立证据）。
	if err := s.writeFileList(ctx, tw, manifest); err != nil {
		return nil, err
	}

	// 4) manifest（最后写：对象 sha256 已齐）。
	if err := writeJSONEntry(tw, ManifestName, manifest); err != nil {
		return nil, err
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("备份包写入结束失败: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("备份包压缩结束失败: %w", err)
	}
	return manifest, nil
}

// writeObject 把一个对象写进包并算出摘要。
func (s *Service) writeObject(ctx context.Context, tw *tar.Writer, path string) (ObjectEntry, error) {
	// 包内条目名：objects/<相对路径>；相对路径自身已由存储层保证不逃逸。
	name := ObjectDirName + strings.TrimPrefix(path, "/")
	if _, err := safeEntryName(name); err != nil {
		return ObjectEntry{}, err
	}

	rc, size, err := s.store.GetFileReader(ctx, path)
	if err != nil {
		return ObjectEntry{}, fmt.Errorf("读取对象失败 %q: %w", path, err)
	}
	defer func() { _ = rc.Close() }()

	hasher := sha256Hasher()
	hdr := &tar.Header{
		Name:     name,
		Mode:     0o600,
		Size:     size,
		Typeflag: tar.TypeReg,
		ModTime:  time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return ObjectEntry{}, fmt.Errorf("写对象头失败 %q: %w", path, err)
	}
	written, err := io.Copy(io.MultiWriter(tw, hasher), rc)
	if err != nil {
		return ObjectEntry{}, fmt.Errorf("写对象失败 %q: %w", path, err)
	}
	if written != size {
		return ObjectEntry{}, fmt.Errorf("对象 %q 大小不一致（声明 %d 实得 %d）", path, size, written)
	}
	return ObjectEntry{Path: path, Size: written, SHA256: hexOf(hasher)}, nil
}

// writeDBSnapshot 写 DB 一致快照（当前仅 SQLite：VACUUM INTO）。
func (s *Service) writeDBSnapshot(ctx context.Context, tw *tar.Writer, manifest *Manifest) error {
	if s.dbType != "sqlite" {
		return ErrUnsupportedDB
	}
	dir, err := os.MkdirTemp("", "pb-backup-*")
	if err != nil {
		return fmt.Errorf("创建快照临时目录失败: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// VACUUM INTO 的目标**必须不存在** ⇒ 放在新建的空目录里。
	target := filepath.Join(dir, "backup.db")
	if err := s.db.WithContext(ctx).Exec("VACUUM INTO ?", target).Error; err != nil {
		return fmt.Errorf("生成数据库一致快照失败: %w", err)
	}
	f, err := os.Open(target)
	if err != nil {
		return fmt.Errorf("打开数据库快照失败: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("读取数据库快照信息失败: %w", err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:     DBEntryName,
		Mode:     0o600,
		Size:     info.Size(),
		Typeflag: tar.TypeReg,
		ModTime:  info.ModTime(),
	}); err != nil {
		return fmt.Errorf("写数据库快照头失败: %w", err)
	}
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("写数据库快照失败: %w", err)
	}
	return nil
}

// writeFileList 写 files.json（分享/取件码清单）。
func (s *Service) writeFileList(ctx context.Context, tw *tar.Writer, manifest *Manifest) error {
	var rows []model.FileCode
	if err := s.db.WithContext(ctx).Order("id asc").Find(&rows).Error; err != nil {
		return fmt.Errorf("列举分享记录失败: %w", err)
	}
	list := FileList{Count: len(rows), Files: make([]FileEntry, 0, len(rows))}
	for _, r := range rows {
		e := FileEntry{
			ID:         r.ID,
			Code:       r.Code,
			FilePath:   r.FilePath,
			Size:       r.Size,
			Status:     r.Status,
			UsedCount:  r.UsedCount,
			UUIDFileNm: r.UUIDFileName,
		}
		if r.ExpiredAt != nil {
			e.ExpiredAt = r.ExpiredAt.Format(time.RFC3339)
		}
		list.Files = append(list.Files, e)
	}
	manifest.FileCount = int64(list.Count)
	return writeJSONEntry(tw, FileListName, list)
}

// writeJSONEntry 写一个 JSON 条目。
func writeJSONEntry(tw *tar.Writer, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 %s 失败: %w", name, err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o600,
		Size:     int64(len(b)),
		Typeflag: tar.TypeReg,
		ModTime:  time.Now(),
	}); err != nil {
		return fmt.Errorf("写 %s 头失败: %w", name, err)
	}
	if _, err := tw.Write(b); err != nil {
		return fmt.Errorf("写 %s 失败: %w", name, err)
	}
	return nil
}
