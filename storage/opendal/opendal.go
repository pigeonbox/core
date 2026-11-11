// Package opendal 提供 OpenDAL 风格的存储抽象。
//
// 为什么不用真正的 OpenDAL Go binding？
//  1. 真正的 OpenDAL Go binding（github.com/apache/opendal/bindings/go）只提供
//     Linux 预编译库（libopendal_c.linux.amd64.so.zst），macOS 编译会失败：
//     `undefined: libopendalZst`
//  2. 需 libffi 系统库 + 单独安装每个 scheme 的 companion module（s3/oss/cos/...）
//  3. 部署复杂度高，团队上手成本大
//
// 解决方案：
//   - OpenDAL 风格 API（Read/Write/Stat/Delete/List/Copy/Rename/CreateDir/RemoveAll）
//   - scheme + options 统一配置（OpenDAL 风格）
//   - 内部 dispatch：fs 走本地文件系统；s3/webdav 由 Driver 接口驱动
//     （s3 → minio-go，webdav → gowebdav，见 s3.go / webdav.go）
//   - 当真正 OpenDAL Go binding 能在 macOS/Windows 跑通时，可替换 backend
//     实现（保持 interface 不变）
//
// 支持的 scheme（OpenDAL 命名约定）:
//
//	fs       - 本地文件系统
//	s3       - AWS S3 / 兼容 S3 协议（MinIO/Ceph/OSS/COS/B2/...）
//	webdav   - WebDAV（含坚果云/Nextcloud/Alist）
//	oss/cos/obs/azblob/gcs/sftp/hdfs/memory - 扩展点，未配置驱动时按 fs 兜底
package opendal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oekit/kitx/uidgen"
)

// Scheme 存储方案（OpenDAL 命名约定）
type Scheme string

const (
	SchemeFS       Scheme = "fs"
	SchemeS3       Scheme = "s3"
	SchemeOSS      Scheme = "oss"
	SchemeCOS      Scheme = "cos"
	SchemeOBS      Scheme = "obs"
	SchemeAzBlob   Scheme = "azblob"
	SchemeGCS      Scheme = "gcs"
	SchemeWebDAV   Scheme = "webdav"
	SchemeSFTP     Scheme = "sftp"
	SchemeFTP      Scheme = "ftp"
	SchemeHDFS     Scheme = "hdfs"
	SchemeOneDrive Scheme = "onedrive"
	SchemeMemory   Scheme = "memory"
)

// supportedSchemes 当前 backend 实际支持的 scheme。
//
// 云厂商（oss/cos/bos/ks3/obs）不在此表、也不走本包的原生驱动扩展点：
// 上层 storage.buildOperator 已将其统一映射为 SchemeS3 驱动
// （各厂商官方提供 S3 兼容端点，endpoint 由 storage/providers.go 按厂商+region
// 推导）。因此本包只见 fs/s3/webdav 三种 scheme。
// 原生驱动（oss.go/cos.go 用各厂商 native API）仍为可选扩展点：仅当需要
// S3 兼容协议不覆盖的厂商私有功能时才值得实现。
var supportedSchemes = map[Scheme]bool{
	SchemeFS:       true,
	SchemeS3:       true,
	SchemeWebDAV:   true,
	SchemeFTP:      true,
	SchemeSFTP:     true,
	SchemeAzBlob:   true,
	SchemeHDFS:     true,
	SchemeOneDrive: true,
	// memory：未实现，需时实现 Driver 接口扩展
}

// Driver 远端 scheme（s3/webdav 等）的底层驱动接口。
// Operator 在 New 时按 scheme 构造具体驱动；NewCustom 允许测试注入假驱动。
// key 一律为相对路径（无前导 /，不含 bucket/远端 root 前缀）。
type Driver interface {
	Write(ctx context.Context, key string, data []byte) error
	// WriteStream 流式写入；size 为确切字节数（调用方先 Stat 求和得到）
	WriteStream(ctx context.Context, key string, r io.Reader, size int64) error
	Read(ctx context.Context, key string) ([]byte, error)
	Reader(ctx context.Context, key string) (io.ReadCloser, error)
	Stat(ctx context.Context, key string) (*Metadata, error)
	Delete(ctx context.Context, key string) error
	// RemoveAll 递归删除 key 前缀/目录；实现必须拒绝空 key（防全量误删）
	RemoveAll(ctx context.Context, key string) error
}

// 可选能力接口：驱动按需实现，Operator 做类型断言后增强行为。
type (
	mkdirAller interface {
		MkdirAll(ctx context.Context, key string) error
	}
	lister interface {
		List(ctx context.Context, key string) ([]*Metadata, error)
	}
	copier interface {
		Copy(ctx context.Context, src, dst string) error
	}
	renamer interface {
		Rename(ctx context.Context, src, dst string) error
	}
	presigner interface {
		Presign(ctx context.Context, method, key string, expire time.Duration, opts map[string]string) (*PresignedResult, error)
	}
	rangeReader interface {
		// ReadRange 读取 [start, start+length) 字节区间；length<=0 表示读到末尾。
		// 不支持区间读的驱动不实现此接口（Operator.ReadRange 返回 ErrRangeUnsupported）。
		ReadRange(ctx context.Context, key string, start, length int64) (io.ReadCloser, error)
	}
)

// Metadata 文件元数据
type Metadata struct {
	Path    string
	Size    int64
	IsDir   bool
	ModTime time.Time
	ETag    string
}

// PresignedRequest 预签名请求
type PresignedRequest struct {
	Path   string            // 对象 key
	Method string            // PUT / GET
	Expire time.Duration     // 过期时间
	Opts   map[string]string // 额外选项（content-type, content-md5, ...）
}

// PresignedResult 预签名结果
type PresignedResult struct {
	URL     string            // 预签名 URL
	Method  string            // HTTP method
	Headers map[string]string // 需要附加的 header
	Expire  time.Time         // 过期时间
}

// Operator 存储操作器（OpenDAL 风格）
type Operator struct {
	scheme  Scheme
	root    string
	options map[string]string
	mu      sync.RWMutex

	// driver 远端 scheme 的底层驱动；scheme=fs 时为 nil
	driver Driver

	// 内部使用（未来扩展点）
	presignEnabled bool
}

// Config 构造配置
type Config struct {
	Scheme  Scheme
	Root    string            // scheme=fs 时为本地路径；scheme=s3 时为 bucket 名
	Options map[string]string // 通用 options（按 scheme 解释，见 s3.go/webdav.go）
}

// New 创建 Operator。
//
// s3/webdav 的连接参数缺失/非法时返回 error——不再静默回退 fs，
// 「配置保存成功但实际读写落本地盘」正是拆分前遗留的假开关问题。
// 未知 scheme 仍按 fs 兜底（真正 OpenDAL binding 接入后改为返回 error）。
func New(cfg Config) (*Operator, error) {
	if cfg.Scheme == "" {
		cfg.Scheme = SchemeFS
	}
	if cfg.Options == nil {
		cfg.Options = map[string]string{}
	}
	op := &Operator{
		scheme:         cfg.Scheme,
		root:           cfg.Root,
		options:        cfg.Options,
		presignEnabled: cfg.Scheme == SchemeS3 || cfg.Scheme == SchemeOSS || cfg.Scheme == SchemeCOS,
	}
	switch cfg.Scheme {
	case SchemeS3:
		opts := cfg.Options
		if opts["bucket"] == "" && cfg.Root != "" {
			opts["bucket"] = cfg.Root // Root 约定为 bucket 名（OpenDAL 风格）
		}
		d, err := newS3Driver(opts)
		if err != nil {
			return nil, err
		}
		op.driver = d
	case SchemeWebDAV:
		d, err := newWebDAVDriver(cfg.Options)
		if err != nil {
			return nil, err
		}
		op.driver = d
	case SchemeFTP:
		d, err := newFTPDriver(cfg.Options)
		if err != nil {
			return nil, err
		}
		op.driver = d
	case SchemeSFTP:
		d, err := newSFTPDriver(cfg.Options)
		if err != nil {
			return nil, err
		}
		op.driver = d
	case SchemeAzBlob:
		d, err := newAzBlobDriver(cfg.Options)
		if err != nil {
			return nil, err
		}
		op.driver = d
	case SchemeHDFS:
		d, err := newHDFSDriver(cfg.Options)
		if err != nil {
			return nil, err
		}
		op.driver = d
	case SchemeOneDrive:
		d, err := newOneDriveDriver(cfg.Options)
		if err != nil {
			return nil, err
		}
		op.driver = d
	default:
		if !supportedSchemes[cfg.Scheme] {
			// 未知 scheme：按 fs 兜底（保持既有宽容行为）
			op.scheme = SchemeFS
			op.presignEnabled = false
		}
	}
	return op, nil
}

// NewCustom 用外部注入的驱动创建 Operator（测试专用：注入假驱动验证分派逻辑）。
func NewCustom(scheme Scheme, d Driver) *Operator {
	if scheme == "" {
		scheme = SchemeFS
	}
	return &Operator{
		scheme:         scheme,
		options:        map[string]string{},
		driver:         d,
		presignEnabled: scheme == SchemeS3 || scheme == SchemeOSS || scheme == SchemeCOS,
	}
}

// Scheme 返回当前 scheme
func (op *Operator) Scheme() Scheme { return op.scheme }

// resolvePath 把相对 path 解析为 fs backend 实际路径
func (op *Operator) resolvePath(p string) string {
	p = strings.TrimPrefix(p, "/")
	return filepath.Join(op.root, p)
}

// key 把相对 path 规范为远端驱动的 key（去前导 /；bucket/root 前缀由驱动负责）
func (op *Operator) key(p string) string {
	return strings.TrimPrefix(p, "/")
}

// ============ 基础文件操作（OpenDAL 风格 API）============

// Write 写入数据
func (op *Operator) Write(ctx context.Context, path string, data []byte) error {
	if op.scheme == SchemeFS {
		fullPath := op.resolvePath(path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			return fmt.Errorf("mkdir: %w", err)
		}
		return os.WriteFile(fullPath, data, 0644)
	}
	return op.driver.Write(ctx, op.key(path), data)
}

// WriteStream 流式写入（size 为确切字节数；fs 直接 create+copy）
func (op *Operator) WriteStream(ctx context.Context, path string, r io.Reader, size int64) error {
	if op.scheme == SchemeFS {
		fullPath := op.resolvePath(path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			return fmt.Errorf("mkdir: %w", err)
		}
		f, err := os.Create(fullPath)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(f, r)
		return err
	}
	return op.driver.WriteStream(ctx, op.key(path), r, size)
}

// Read 读取整个文件（限量整读，core#118：超 MaxWholeReadBytes 显式报错）
func (op *Operator) Read(ctx context.Context, path string) ([]byte, error) {
	if op.scheme == SchemeFS {
		f, err := os.Open(op.resolvePath(path))
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		return ReadAllLimited(f, MaxWholeReadBytes)
	}
	return op.driver.Read(ctx, op.key(path))
}

// Reader 获取流式读取器
func (op *Operator) Reader(ctx context.Context, path string) (io.ReadCloser, error) {
	if op.scheme == SchemeFS {
		return os.Open(op.resolvePath(path))
	}
	return op.driver.Reader(ctx, op.key(path))
}

// ErrRangeUnsupported 当前驱动不支持按字节区间读取。
// 调用方（Range 下载/断点续传）应回退全量流式（200）。
var ErrRangeUnsupported = errors.New("storage driver: range read unsupported")

// ReadRange 读取 [start, start+length) 字节区间（断点续传/Range 下载）。
// length<=0 表示读到末尾。驱动未实现区间读能力时返回 ErrRangeUnsupported。
func (op *Operator) ReadRange(ctx context.Context, path string, start, length int64) (io.ReadCloser, error) {
	if op.scheme == SchemeFS {
		f, err := os.Open(op.resolvePath(path))
		if err != nil {
			return nil, err
		}
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
		if length > 0 {
			return struct {
				io.Reader
				io.Closer
			}{io.LimitReader(f, length), f}, nil
		}
		return f, nil
	}
	rr, ok := op.driver.(rangeReader)
	if !ok {
		return nil, ErrRangeUnsupported
	}
	return rr.ReadRange(ctx, op.key(path), start, length)
}

// Stat 获取文件元信息；对象不存在时返回包装了 os.ErrNotExist 的错误
func (op *Operator) Stat(ctx context.Context, path string) (*Metadata, error) {
	if op.scheme == SchemeFS {
		info, err := os.Stat(op.resolvePath(path))
		if err != nil {
			return nil, err
		}
		return &Metadata{
			Path:    path,
			Size:    info.Size(),
			IsDir:   info.IsDir(),
			ModTime: info.ModTime(),
		}, nil
	}
	return op.driver.Stat(ctx, op.key(path))
}

// Delete 删除文件
func (op *Operator) Delete(ctx context.Context, path string) error {
	if op.scheme == SchemeFS {
		return os.Remove(op.resolvePath(path))
	}
	return op.driver.Delete(ctx, op.key(path))
}

// Exists 检查文件是否存在
func (op *Operator) Exists(ctx context.Context, path string) bool {
	_, err := op.Stat(ctx, path)
	return err == nil
}

// CreateDir 创建目录（对象存储无目录概念时为 no-op）
func (op *Operator) CreateDir(ctx context.Context, path string) error {
	if op.scheme == SchemeFS {
		return os.MkdirAll(op.resolvePath(path), 0755)
	}
	if m, ok := op.driver.(mkdirAller); ok {
		return m.MkdirAll(ctx, op.key(path))
	}
	return nil
}

// RemoveAll 递归删除
func (op *Operator) RemoveAll(ctx context.Context, path string) error {
	if op.scheme == SchemeFS {
		return os.RemoveAll(op.resolvePath(path))
	}
	return op.driver.RemoveAll(ctx, op.key(path))
}

// List 列出目录下的所有条目（驱动未实现时返回错误）
func (op *Operator) List(ctx context.Context, path string) ([]*Metadata, error) {
	if op.scheme == SchemeFS {
		fullPath := op.resolvePath(path)
		entries, err := os.ReadDir(fullPath)
		if err != nil {
			return nil, err
		}
		var result []*Metadata
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			rel, _ := filepath.Rel(op.root, filepath.Join(fullPath, e.Name()))
			result = append(result, &Metadata{
				Path:    rel,
				Size:    info.Size(),
				IsDir:   info.IsDir(),
				ModTime: info.ModTime(),
			})
		}
		return result, nil
	}
	if l, ok := op.driver.(lister); ok {
		return l.List(ctx, op.key(path))
	}
	return nil, errors.New("list not implemented for scheme: " + string(op.scheme))
}

// Copy 复制文件。
//
// FS 分支**流式**拷贝（core#140，core#118 同族残留收口）：原 os.ReadFile 把源
// 文件整读进内存再写出——本地→本地拷贝峰值内存 ≈ 文件大小（无界整读面）。
// 拷贝不是「取内容」消费，**刻意不套** MaxWholeReadBytes 整读上限（封顶会砍掉
// 合法大文件拷贝），改为打开两端 io.Copy 流式推进：峰值内存 = io.Copy 的固定
// 缓冲，与文件大小无关。
func (op *Operator) Copy(ctx context.Context, src, dst string) error {
	if op.scheme == SchemeFS {
		srcPath := op.resolvePath(src)
		dstPath := op.resolvePath(dst)
		in, err := os.Open(srcPath)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
			return err
		}
		out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	}
	if c, ok := op.driver.(copier); ok {
		return c.Copy(ctx, op.key(src), op.key(dst))
	}
	return errors.New("copy not implemented for scheme: " + string(op.scheme))
}

// Rename 重命名/移动
func (op *Operator) Rename(ctx context.Context, src, dst string) error {
	if op.scheme == SchemeFS {
		srcPath := op.resolvePath(src)
		dstPath := op.resolvePath(dst)
		if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
			return err
		}
		return os.Rename(srcPath, dstPath)
	}
	if r, ok := op.driver.(renamer); ok {
		return r.Rename(ctx, op.key(src), op.key(dst))
	}
	return errors.New("rename not implemented for scheme: " + string(op.scheme))
}

// ============ 预签名 URL（OpenDAL 风格）============

// Presign 生成预签名 URL。
//
// s3：由 minio-go 离线签名生成真实 S3 预签名 URL（客户端可直传直下）。
// 其余 scheme：回退为拼自家直传 URL（走自家服务中转，见 presign 域）。
func (op *Operator) Presign(ctx context.Context, req PresignedRequest) (*PresignedResult, error) {
	if req.Expire == 0 {
		req.Expire = 1 * time.Hour
	}
	if req.Method == "" {
		req.Method = "PUT"
	}
	expireAt := time.Now().Add(req.Expire)

	if p, ok := op.driver.(presigner); ok {
		res, err := p.Presign(ctx, req.Method, op.key(req.Path), req.Expire, req.Opts)
		if err != nil {
			return nil, err
		}
		if res.Method == "" {
			res.Method = req.Method
		}
		res.Expire = expireAt
		return res, nil
	}

	if !op.presignEnabled {
		// fallback: 用 base_url + token 拼成自家直传 URL
		baseURL := op.options["base_url"]
		if baseURL == "" {
			baseURL = "http://localhost:12345"
		}
		token, err := genToken()
		if err != nil {
			return nil, fmt.Errorf("presign 上传令牌生成失败（随机源不可用）: %w", err)
		}
		url := fmt.Sprintf("%s/api/v1/presign/upload-direct?path=%s&token=%s", baseURL, req.Path, token)
		return &PresignedResult{
			URL:     url,
			Method:  req.Method,
			Headers: map[string]string{"X-Upload-Token": token},
			Expire:  expireAt,
		}, nil
	}

	return nil, errors.New("presign not implemented for scheme: " + string(op.scheme))
}

// ============ 辅助函数 ============

// Probe 认证级连通性验证：s3 校验凭据+桶存在；webdav 校验根路径可达；
// 其他 scheme（含 fs）直接通过。管理端切换/保存存储配置前调用。
func Probe(ctx context.Context, op *Operator) error {
	if p, ok := op.driver.(interface {
		Probe(ctx context.Context) error
	}); ok {
		return p.Probe(ctx)
	}
	return nil
}

// presignTokenEntropyBytes 自家直传回退通道上传令牌的熵预算：16 字节 = 128 bit
// 熵 ⇒ 2^128 防伪造预算（core#86 收编 kitx/uidgen 时定长于此）。
const presignTokenEntropyBytes = 16

// genToken presign 回退通道（presignEnabled=false）的直传上传令牌。
//
// 随机源失败**显式上抛**（core#86：原实现吞错 ⇒ 熵源故障时恒发全 0 令牌 ⇒
// 任何客户端可伪造 X-Upload-Token 直传）——调用方 Presign 收到错误即拒绝签发。
func genToken() (string, error) {
	return uidgen.RandomHexE(presignTokenEntropyBytes)
}

// DefaultOperator 全局默认 operator
var (
	defaultOp     *Operator
	defaultOpOnce sync.Once
)

// SetDefault 设置默认 operator
func SetDefault(op *Operator) {
	defaultOp = op
}

// GetDefault 获取默认 operator
func GetDefault() *Operator {
	if defaultOp == nil {
		defaultOpOnce.Do(func() {
			defaultOp, _ = New(Config{
				Scheme: SchemeFS,
				Root:   "./data",
				Options: map[string]string{
					"base_url": "http://localhost:12345",
				},
			})
		})
	}
	return defaultOp
}
