// service.go Service 骨架与接线：结构体定义/构造/注入 Setter/审计入口/包级单例。
//
// 2026-10-10 站点配置中心独立为 app/config 域：本域只保留治理职责
// （用户/文件/统计/维护/认证/审计）与管理面 API 的配置门面委派（config.go）。
package admin

import (
	"context"
	"sync"

	appconfig "github.com/pigeonbox/core/app/config"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
	"gorm.io/gorm"
)

type Service struct {
	userRepo           *dao.UserRepository
	fileCodeRepo       *dao.FileCodeRepository
	transferLogRepo    *dao.TransferLogRepository
	adminOperationRepo *dao.AdminOperationLogRepository
	chunkRepo          *dao.ChunkRepository
	fileFileRepo       *dao.FileCodeFileRepository // 多文件子表（files/maintenance 消费）
	sysConfigRepo      *dao.SystemConfigRepository // 仅 OptimizeDatabase 表维护用（配置读写归 app/config 域）
	storage            storage.StorageInterface
	federation         FederationNotifier // P2P 联邦口令路由钩子（nil = 非联邦模式；读写走 federationMu——P1 后台热切换=运行期写）
	federationMu       sync.RWMutex

	// cfg 站点配置中心（app/config 域实例；本域经 config.go 门面委派）。
	// NewService() 持独立实例（测试隔离）；Default() 单例改绑 config.Default()
	// ——system_configs 单行读改写语义要求全站唯一配置实例（2026-10-03 事故）。
	cfg *appconfig.Service
}

// FederationNotifier P2P 联邦撤销钩子（窄接口，实现在 app/federation；
// bootstrap 注入。admin 删除路径独立于 share 域，须单独挂钩——否则管理端
// 删除的分享在联邦里残留到公告 TTL 过期）。
type FederationNotifier interface {
	ShareDeleted(code string)
}

// SetFederationNotifier 注入联邦撤销钩子（bootstrap 调用；nil = 非联邦。
// P1 后台热切换后为运行期写，与删除路径并发——须加锁）
func (s *Service) SetFederationNotifier(f FederationNotifier) {
	s.federationMu.Lock()
	defer s.federationMu.Unlock()
	s.federation = f
}

// currentFederation 读当前联邦钩子（并发安全快照）
func (s *Service) currentFederation() FederationNotifier {
	s.federationMu.RLock()
	defer s.federationMu.RUnlock()
	return s.federation
}

// LogOperation 管理操作审计公开入口（供 gen handler 等非本域包写审计，
// 与 logAdminOperation 同表同语义；P2a 设备直传操作页使用）。
func (s *Service) LogOperation(ctx context.Context, action, target string, success bool) {
	s.logAdminOperation(ctx, action, target, success)
}

func NewService(gormDB *gorm.DB) *Service {
	return &Service{
		userRepo:           dao.NewUserRepository(gormDB),
		fileCodeRepo:       dao.NewFileCodeRepository(gormDB),
		transferLogRepo:    dao.NewTransferLogRepository(gormDB),
		adminOperationRepo: dao.NewAdminOperationLogRepository(gormDB),
		chunkRepo:          dao.NewChunkRepository(gormDB),
		fileFileRepo:       dao.NewFileCodeFileRepository(gormDB),
		sysConfigRepo:      dao.NewSystemConfigRepository(gormDB),
		cfg:                appconfig.NewService(gormDB),
	}
}

// SetStorage 注入存储服务（用于过期清理删物理文件）
func (s *Service) SetStorage(st storage.StorageInterface) {
	s.storage = st
}

// logAdminOperation 写入管理操作审计日志（P0 修复：模型/DAO 早已有之，
// 此前从未接线——后台所有变更操作零审计）。审计失败只记运行日志，不阻断业务。
// 操作者从 hertz 请求上下文提取（middleware.AuditActor 单源，config 域同款）；
// 定时任务等非请求场景记为 system。
func (s *Service) logAdminOperation(ctx context.Context, action, target string, success bool) {
	actorID, actorName, ip := middleware.AuditActor(ctx)
	entry := &model.AdminOperationLog{
		Action:    action,
		Target:    target,
		Success:   success,
		ActorID:   actorID,
		ActorName: actorName,
		IP:        ip,
	}
	if err := s.adminOperationRepo.Create(ctx, entry); err != nil {
		logger.CtxWarn(ctx, "admin audit log write failed", "action", action, "err", err)
	}
}

// Audit 管理端操作审计的统一入口（transport 侧审计写入收口到此，消除与
// service 层的孪生实现；经全站唯一 Default() 实例落库）。
func Audit(ctx context.Context, action, target string, success bool) {
	Default().logAdminOperation(ctx, action, target, success)
}

// defaultSvc 包级单例（InitDefault 显式初始化）。
var (
	defaultMu  sync.RWMutex
	defaultSvc *Service
)

// InitDefault 注册全站 admin 单例（bootstrap 在 config.InitDefault 之后调用；幂等）。
// 全站单例改绑配置中心单例：admin.Default() 与 config.Default() 共享
// 同一份配置内存缓存与 DB 写通道（单行读改写约束，见 app/config 包注释）。
func InitDefault(gormDB *gorm.DB) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultSvc == nil {
		defaultSvc = NewService(gormDB)
		defaultSvc.cfg = appconfig.Default()
	}
}

// Default 全站共享的 admin Service 实例（治理面 + 配置门面同一实例；
// 未 InitDefault 直接取用是装配错误，fail-loud）。
func Default() *Service {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	if defaultSvc == nil {
		panic("admin: Default() before InitDefault——bootstrap 必须在 InitDatabase 后调 InitDefault")
	}
	return defaultSvc
}
