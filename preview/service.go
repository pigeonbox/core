package preview

import (
	"fmt"
	"log"
	"os"
)

// svc 预览服务全局单例。
// 设计权衡：bootstrap 调 InitService 初始化，各处 GetService 读取。预览功能可选
// （依赖 ffmpeg），用全局单例 + nil 判断简化调用方。未来若迁移到 DI 容器可改为构造注入。
var svc *Service

// InitService 初始化预览服务
func InitService(cfg *Config) error {
	if cfg == nil {
		cfg = &Config{
			EnablePreview:    true,
			ThumbnailWidth:   300,
			ThumbnailHeight:  200,
			MaxFileSize:      50 * 1024 * 1024, // 50MB
			PreviewCachePath: "./data/previews",
			FFmpegPath:       "ffmpeg",
		}
	}

	// 确保缓存目录存在
	if err := os.MkdirAll(cfg.PreviewCachePath, 0755); err != nil {
		return fmt.Errorf("failed to create preview cache directory: %w", err)
	}

	svc = NewService(cfg)

	// 注册生成器
	svc.RegisterGenerator(PreviewTypeImage, NewImageGenerator(cfg))
	svc.RegisterGenerator(PreviewTypeVideo, NewVideoGenerator(cfg))
	svc.RegisterGenerator(PreviewTypeAudio, NewAudioGenerator(cfg))
	svc.RegisterGenerator(PreviewTypeCode, NewCodeGenerator(cfg))

	log.Println("Preview service initialized successfully")
	return nil
}

// GetService 获取预览服务实例。
//
// 2026-10-28（真机实证：fnOS 4.0.7，每次启动报 "failed to create preview cache
// directory: mkdir ./data/previews: permission denied"）：此前 svc==nil 时会
// **静默**用默认配置（缓存目录 "./data/previews"，按进程 CWD 解析）重试初始化——
// 重试在 NAS 包/容器/systemd 部署下必然失败（缓存目录不可建），于是调用方拿到
// nil 却毫无线索；更糟的是 MaxFileSize() 的 nil 接收者分支返回 0（=不限），
// 把 2026-10-08 加固的「预览大小上限」静默关掉 ⇒ 任意大文件仍会全量落临时盘
// （磁盘耗尽 DoS 面回归）。
//
// 改为：不再自动重试；未注入即返回 nil，由调用方 fail-fast 报明确错误
// （与仓内 getShareService() 对残废实例的处理同构）。
func GetService() *Service {
	return svc
}

// SetService 注入预览服务单例（装配期由 bootstrap.InitService 间接设置，
// 测试期可注入桩或置 nil 以覆盖「引擎不可用」路径）。
func SetService(s *Service) {
	svc = s
}

// MaxFileSize 返回预览大小上限（字节，0=不限）。调用方应在打开对象前用
// DB 记录的文件大小预判——否则任意大文件会先被全量拉到临时盘再由生成器
// 拒绝，构成磁盘耗尽攻击面（2026-10-08 加固）。
func (s *Service) MaxFileSize() int64 {
	if s == nil || s.config == nil {
		return 0
	}
	return s.config.MaxFileSize
}
