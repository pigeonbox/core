package bootstrap

import (
	"context"
	"fmt"
	"io"

	adminApp "github.com/pigeonbox/core/app/admin"
	"github.com/pigeonbox/core/app/backup"
	configApp "github.com/pigeonbox/core/app/config"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/storage"
	"gorm.io/gorm"
)

// NewOfflineBackupService 构造**离线**备份服务（导出 / 恢复用；不启动 HTTP 服务）。
//
// # 为什么是「离线」而不是管理面端点
//
// 导出与恢复天然是**同机**操作（要读本地 DB 与本地存储）：
//
//	· 做成 HTTP 端点 ⇒ 多一层「服务端正在运行 ⇒ 恢复会砸自己的脚」的风险；
//	· `pb` 是**纯 HTTP 客户端**（刻意不链接 core）⇒ 让 `pb backup` 落地就得把
//	  core 拖进这个轻量 CLI（体积 + 数十 MB）。
//
// ⇒ 形态定为**同机离线子命令**（server 二进制：`pigeonbox backup export` /
// `pigeonbox restore`），与「恢复必须停服」的事实一致。
//
// # 前置
//
// 调用方必须先完成：InitConfig → conf.SetGlobalConfig → InitDatabase
// （否则 DB 与存储后端都没就绪）。本函数只做**装配**，不负责初始化。
func NewOfflineBackupService(cfg *conf.AppConfiguration, database *gorm.DB) (*backup.Service, error) {
	if database == nil {
		return nil, fmt.Errorf("数据库未初始化（须先 InitConfig + InitDatabase）")
	}
	// admin 域的单例必须先初始化（Default() 在未初始化时**故意 panic**——fail-loud，
	// 免得静默拿到一个没有 DB 的空服务）。正常启动路径里这一步在 InitDatabase 之后。
	// 顺序与 bootstrap/wiring.go 一致：config 域先、admin 域后
	// （admin.Default() 内部要取 config.Default()）。
	configApp.InitDefault(database)
	adminApp.InitDefault(database)
	// 对象清单来源 = admin 域（含软删记录 ⇒ 软删分享可恢复，它的对象也得带走）。
	adminSvc := adminApp.Default()

	// 对象读写：**自建**一个存储服务（不走 bootstrap 的包级单例）。
	//
	// 为什么不用单例：单例是**进程内**共享的，且依赖 bootstrap 启动期的包级状态
	// （config 变量等）⇒ 离线进程里取它要么 panic，要么拿到一个与运行中实例
	// 混在一起的半初始化对象。而备份/恢复本来就该是**独立进程**：自建一份，
	// 语义干净（读什么后端、写到哪个后端，都由本次进程自己的配置决定）。
	storeCfg := storage.ConfigFromConf(&cfg.Storage, offlineBaseURL(cfg))
	store, err := storage.NewStorageServiceE(storeCfg)
	if err != nil {
		// 远端后端连不上 ⇒ 降级 local（与 bootstrap 同款：连不上时用 local 兜底，
		// 且**如实记录**：导出结果里的 storage_type 会显示实际生效的类型）。
		store = storage.NewStorageService(storeCfg)
	}
	return backup.NewService(database, adminSvc, storageTypeAdapter{store}), nil
}

// offlineBaseURL 对外基础地址（server.base_url 优先，否则 host:port）。
func offlineBaseURL(cfg *conf.AppConfiguration) string {
	if cfg.Server.BaseURL != "" {
		return cfg.Server.BaseURL
	}
	return fmt.Sprintf("http://%s:%d", cfg.Server.Host, cfg.Server.Port)
}

// storageTypeAdapter 把存储单例适配成备份域的窄接口
// （单例的 EffectiveType 返回自己的类型 ⇒ 这里转成字符串，备份域不必依赖它）。
type storageTypeAdapter struct {
	svc *storage.StorageService
}

func (a storageTypeAdapter) GetFileReader(ctx context.Context, p string) (io.ReadCloser, int64, error) {
	return a.svc.GetFileReader(ctx, p)
}

func (a storageTypeAdapter) SaveStream(ctx context.Context, p string, r io.Reader, size int64) (int64, error) {
	return a.svc.SaveStream(ctx, p, r, size)
}

func (a storageTypeAdapter) EffectiveType() string { return string(a.svc.EffectiveType()) }
