package utils

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/metrics"
)

// ErrFileTooLarge 文件超过允许大小
var ErrFileTooLarge = errors.New("file size exceeds limit")

// FileTooLargeError 文件超过整文件大小上限（upload.max_file_size）。
//
// 带业务码（ErrCode）⇒ handler 的 resp.NewTypedError 透传 CodeTooLarge
// （HTTP 413）并保留动态文案「文件过大: 最大允许 N 字节」；前端已把 10010
// 列入动态文案白名单，直接显示后端原文。
//
// 2026-10-23 背景：此前各处用 fmt.Errorf("文件过大: ...") 包装 ⇒ 退化成普通
// error，业务码丢失——presign init 落 30008「预签名生成失败」，把后端清晰的
// 文案抹掉（前端按错误码映射覆盖）；chunk init 更糟，落 500「服务器内部错误」。
type FileTooLargeError struct{ Limit int64 }

func (e *FileTooLargeError) Error() string {
	return fmt.Sprintf("文件过大: 最大允许 %d 字节", e.Limit)
}

// ErrCode 实现 app 侧 typed error 约定（见 pkg/gate.GateError）。
func (e *FileTooLargeError) ErrCode() int { return errcode.CodeTooLarge }

// NewFileTooLargeError 构造整文件超限错误（带业务码 + 动态文案）。
func NewFileTooLargeError(limit int64) error { return &FileTooLargeError{Limit: limit} }

// ErrFileTypeNotAllowed 文件类型不允许（黑名单命中或白名单未命中）
var ErrFileTypeNotAllowed = errors.New("file type not allowed")

// ErrTextTooLarge 文本分享超过大小上限
var ErrTextTooLarge = errors.New("text share exceeds size limit")

// DefaultTextShareMaxBytes 文本分享默认上限（对齐上游 222KB）。
var DefaultTextShareMaxBytes int64 = 222 * 1024

// GetMaxUploadSize 从全局配置获取上传大小上限。
// 配置未初始化（测试/启动早期）返回 0 表示不限，避免 nil 解引用。
func GetMaxUploadSize() int64 {
	cfg := conf.GetGlobalConfig()
	if cfg == nil {
		return 0
	}
	return cfg.Upload.UploadSize
}

// GetMaxFileSize 整文件大小上限（分片/预签名/匿名登记通道）。
// upload.max_file_size，0 = 不限（分片通道的存在意义即突破单请求限制）。
func GetMaxFileSize() int64 {
	cfg := conf.GetGlobalConfig()
	if cfg == nil {
		return 0
	}
	return cfg.Upload.MaxFileSize
}

// GetTextShareMaxBytes 文本分享大小上限（字节）。未配置时用默认 222KB。
func GetTextShareMaxBytes() int64 {
	cfg := conf.GetGlobalConfig()
	if cfg == nil || cfg.Upload.TextMaxBytes <= 0 {
		return DefaultTextShareMaxBytes
	}
	return cfg.Upload.TextMaxBytes
}

// GetAllowedExtensions 扩展名白名单（小写）。空 = 未启用白名单。
func GetAllowedExtensions() []string {
	cfg := conf.GetGlobalConfig()
	if cfg == nil {
		return nil
	}
	return cfg.Upload.AllowedExtensions
}

// GetEnableMagicCheck 魔数校验开关（upload.enable_magic_check）。
// 配置未初始化（测试/启动早期）按开处理，保持历史安全行为。
func GetEnableMagicCheck() bool {
	cfg := conf.GetGlobalConfig()
	if cfg == nil {
		return true
	}
	return cfg.Upload.EnableMagicCheck
}

// IsAllowedExtension 扩展名准入判定（大小写不敏感），组合白/黑名单：
//   - 白名单非空：扩展名必须命中白名单，否则拒绝（无扩展名视同未命中）
//   - 黑名单（可配置，空则用内置默认）永远生效，命中即拒绝——白名单命中也不能豁免
//
// 魔数检查不在此函数内，由 CheckUploadContent 编排。
func IsAllowedExtension(filename string) bool {
	if IsBlockedExtension(filename, GetBlockedExtensions()) {
		return false
	}
	allowed := GetAllowedExtensions()
	if len(allowed) == 0 {
		return true // 白名单未启用：不在黑名单即放行
	}
	ext := strings.ToLower(filepath.Ext(filename))
	for _, a := range allowed {
		if strings.ToLower(a) == ext {
			return true
		}
	}
	return false
}

// defaultBlockedExtensions 默认拒绝的可执行文件扩展名
var defaultBlockedExtensions = []string{
	".exe", ".bat", ".cmd", ".com", ".scr", ".msi",
	".sh", ".bash", ".ps1", ".vbs", ".js", ".jar",
	".dll", ".so", ".dylib", ".app",
}

// DefaultBlockedExtensions 返回内置默认黑名单扩展名（返回副本，调用方可追加）
func DefaultBlockedExtensions() []string {
	cp := make([]string, len(defaultBlockedExtensions))
	copy(cp, defaultBlockedExtensions)
	return cp
}

// GetBlockedExtensions 生效的黑名单扩展名：upload.blocked_extensions 非空时用配置，
// 否则回退内置默认。黑名单永远参与校验（白名单命中也不能豁免）。
func GetBlockedExtensions() []string {
	cfg := conf.GetGlobalConfig()
	if cfg != nil && len(cfg.Upload.BlockedExtensions) > 0 {
		return cfg.Upload.BlockedExtensions
	}
	return DefaultBlockedExtensions()
}

// IsBlockedExtension 判断文件扩展名是否在黑名单（大小写不敏感）。
// 无扩展名返回 false。
func IsBlockedExtension(filename string, blacklist []string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		return false
	}
	for _, b := range blacklist {
		if strings.ToLower(b) == ext {
			return true
		}
	}
	return false
}

// CheckUploadSize 校验文件大小是否超限。
// maxSize <= 0 表示不限制。
func CheckUploadSize(fileSize, maxSize int64) error {
	if maxSize > 0 && fileSize > maxSize {
		metrics.RecordRejected(metrics.RejectSize)
		return ErrFileTooLarge
	}
	return nil
}

// CheckWholeFileSize 整文件大小上限校验（upload.max_file_size，0=不限）。
// 分片/presign 直传等"整文件"通道用本函数；与单请求体上限 GetMaxUploadSize 区分。
func CheckWholeFileSize(fileSize int64) error {
	return CheckUploadSize(fileSize, GetMaxFileSize())
}
