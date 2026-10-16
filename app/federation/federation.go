// Package federation 将本站点接入 PigeonBox P2P 联邦（github.com/pigeonbox/p2p）。
//
// 职责（M2，设计文档 docs/specs/2026-10-04-p2p-registry-service-design.md §5）：
//   - 节点注册/心跳：Ed25519 签名租约，断线自动重注册 + 全量补公告（自愈）；
//   - 口令联邦路由：share 生命周期钩子公告/撤销 SHA-256(code)→本节点；
//   - 解析代理：GET /api/v1/federation/resolve 转查 registry（浏览器免跨域）。
//
// 信任模型：本包只做"发现"，取件方直连源节点、源节点本地校验口令——零跨节点信任。
// 签名负载格式与 p2p 仓 README 契约逐字节一致（见 client.go 常量注释）。
// 依赖方向：本包不 import 生态任何兄弟模块（叶子约束，core CI dep guard）。
package federation

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/oekit/concurrency/async"
	"go.uber.org/zap"
)

const (
	// nodeTTL 节点租约时长；heartbeatInterval 为其一半，租约平滑续期。
	nodeTTL           = time.Hour
	heartbeatInterval = 30 * time.Minute
	// retryBackoff 心跳失败后的短退避（registry 抖动/重启自愈窗口）。
	retryBackoff = 15 * time.Second
	// announceHorizon 公告滚动视界：registry 公告 max_ttl 默认 168h，
	// 长效分享（含永久）按此视界滚动重公告，实际不过期。
	announceHorizon = 24 * time.Hour
	defaultName     = "PigeonBox"
)

// entry 已公告口令的本地索引（自愈重公告依据；不落库——进程重启后由
// share 事件重新累积，重启窗口内 registry 旧条目靠其自带 TTL 自然过期）。
type entry struct {
	expires   time.Time
	confirmed bool // registry 已确认；失败则下轮心跳重推
}

// Service federation 客户端服务。实现 share.FederationNotifier 窄接口
// （ShareCreated/ShareDeleted），由 bootstrap 注入 share service。
type Service struct {
	cfg conf.FederationConfig
	// regURLs 主备 registry 列表(逗号分隔配置): 写路径全推,读路径依次
	regURLs []string
	nodeID  string
	priv    ed25519.PrivateKey
	name    string
	clients []*registryClient

	mu      sync.Mutex
	entries map[string]*entry // code_hash → entry
	stop    chan struct{}
	stopped sync.Once

	tickOK atomic.Bool // 最近一次心跳结果(退避调度依据)
}

// NewService 构造并加载/生成节点身份密钥。仅在 federation.enabled 时调用；
// 配置不合法返回错误，由 bootstrap 降级为非联邦模式（不影响单站功能）。
func NewService(cfg conf.FederationConfig, nodeName string) (*Service, error) {
	if !cfg.Enabled {
		return nil, errors.New("federation 未启用")
	}
	if strings.TrimSpace(cfg.RegistryURL) == "" {
		return nil, errors.New("federation.registry_url 不能为空")
	}
	if strings.TrimSpace(cfg.PublicURL) == "" {
		return nil, errors.New("federation.public_url 不能为空（公告给取件方的本站可达地址）")
	}
	var bases []string
	for _, raw := range strings.Split(cfg.RegistryURL, ",") {
		raw = strings.TrimSpace(strings.TrimRight(raw, "/"))
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("federation.registry_url 非法: %q", raw)
		}
		bases = append(bases, raw)
	}
	if len(bases) == 0 {
		return nil, errors.New("federation.registry_url 不能为空")
	}
	if cfg.NodeKeyPath == "" {
		cfg.NodeKeyPath = "data/federation.key"
	}
	if cfg.AnnounceMinEntropyBits <= 0 {
		cfg.AnnounceMinEntropyBits = defaultMinEntropyBits
	}
	priv, err := loadOrCreateKey(cfg.NodeKeyPath)
	if err != nil {
		return nil, fmt.Errorf("节点身份密钥: %w", err)
	}
	name := nodeName
	if name == "" {
		name = defaultName
	}
	clients := make([]*registryClient, 0, len(bases))
	for _, b := range bases {
		clients = append(clients, newRegistryClient(b))
	}
	return &Service{
		cfg:     cfg,
		regURLs: bases,
		nodeID:  hex.EncodeToString(priv.Public().(ed25519.PublicKey)),
		priv:    priv,
		name:    name,
		clients: clients,
		entries: make(map[string]*entry),
		stop:    make(chan struct{}),
	}, nil
}

// Enabled 恒 true：实例仅在启用时被构造，handler 侧以 nil 判定未启用。
func (s *Service) Enabled() bool { return true }

// NodeID 本节点联邦身份（Ed25519 公钥 hex）。
func (s *Service) NodeID() string { return s.nodeID }

// Start 启动注册/心跳循环（daemon；与 core 其他后台任务同约定，随进程退出）。
func (s *Service) Start() {
	go s.loop()
}

func (s *Service) loop() {
	s.tick()
	// 初始 timer 按首次 tick 结果:进程启动时 registry 尚不可用是常态
	// (compose 启动顺序/registry 重部署),首启失败必须立即进入 15s 退避,
	// 否则要干等 30m(215 部署实测踩坑)。
	next := heartbeatInterval
	if !s.lastTickOK() {
		next = retryBackoff
	}
	t := time.NewTimer(next)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.tick()
			// 心跳失败短退避(15s)重试,成功恢复常规周期——
			// 固定 30m tick 意味着 registry 抖动后联邦盲窗最长半小时(M4 故障演练修)
			next = heartbeatInterval
			if !s.lastTickOK() {
				next = retryBackoff
			}
			t.Reset(next)
		}
	}
}

// tick 心跳续租 + 全量重公告。多 registry: 写路径全推（主备各自持有全量
// 公告，任一存活即服务解析）；≥1 成功视为健康。失败不致命：短退避重试，
// registry 条目带 TTL 自动过期，最坏情况是联邦可见性延迟一个退避周期。
func (s *Service) tick() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ok := 0
	for i, cl := range s.clients {
		if err := cl.register(ctx, s.nodeID, s.priv, s.cfg.PublicURL, s.name, nodeTTL); err != nil {
			logger.Warn("federation 注册/心跳失败(15s 后重试)", zap.String("registry", s.regURLs[i]), zap.Error(err))
			continue
		}
		ok++
	}
	if ok == 0 {
		s.setLastTickOK(false)
		return
	}
	s.setLastTickOK(true)
	s.announceAll(ctx)
}

func (s *Service) setLastTickOK(ok bool) { s.tickOK.Store(ok) }

func (s *Service) lastTickOK() bool { return s.tickOK.Load() }

// Healthy 最近一次 registry 心跳是否成功（未启动/尚未跑首个周期时为 false）。
// 供管理面（MCP federation_status 等）展示联邦健康度。
func (s *Service) Healthy() bool { return s.lastTickOK() }

// RegistryURLs 主备 registry 基址列表（配置逗号分隔项的副本）。
func (s *Service) RegistryURLs() []string {
	out := make([]string, len(s.regURLs))
	copy(out, s.regURLs)
	return out
}

func (s *Service) announceAll(ctx context.Context) {
	now := time.Now()
	s.mu.Lock()
	live := make(map[string]*entry, len(s.entries))
	for h, e := range s.entries {
		if e.expires.After(now) {
			live[h] = e
		} else {
			delete(s.entries, h)
		}
	}
	s.mu.Unlock()

	ok := 0
	for hash, e := range live {
		expires := e.expires
		if d := time.Until(expires); d > announceHorizon {
			expires = now.Add(announceHorizon)
		}
		delivered := 0
		for _, cl := range s.clients {
			if err := cl.announce(ctx, s.nodeID, s.priv, hash, expires); err != nil {
				continue
			}
			delivered++
		}
		if delivered == 0 {
			logger.Warn("federation 公告待重推", zap.String("hash", shortHash(hash)))
			continue
		}
		// confirmed 写回必须持锁:live 里的 entry 指针与 ShareCreated 的异步
		// 确认 goroutine 共享,这里无锁写与下方锁内写构成数据竞争(-race 实测)
		s.mu.Lock()
		if cur := s.entries[hash]; cur != nil {
			cur.confirmed = true
		}
		s.mu.Unlock()
		ok++
	}
	if len(live) > 0 {
		logger.Info("federation 公告同步完成", zap.Int("total", len(live)), zap.Int("ok", ok), zap.Int("registries", len(s.clients)))
	}
}

// ShareCreated share 生命周期钩子：新分享公告到联邦（异步，registry 抖动
// 不拖慢建分享；失败由心跳循环补发）。熵门槛不过则静默跳过——短数字码
// 不出站，防 registry 侧在线枚举（安全模型见 p2p README）。
func (s *Service) ShareCreated(code string, expiresAt *time.Time) {
	if !announceAllowed(code, s.cfg.AnnounceMinEntropyBits) {
		return
	}
	expires := time.Now().Add(announceHorizon)
	if expiresAt != nil {
		expires = *expiresAt
	}
	hash := codeHash(code)
	s.mu.Lock()
	s.entries[hash] = &entry{expires: expires}
	s.mu.Unlock()

	// 异步公告的 fire-and-forget 协程无监督者，announce 走网络面——GoSafe 兜底
	// 防抖动/驱动异常 panic 带崩进程（失败本就有心跳循环兜底重推）。
	async.GoSafe(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		delivered := 0
		for _, cl := range s.clients {
			if err := cl.announce(ctx, s.nodeID, s.priv, hash, expires); err != nil {
				continue
			}
			delivered++
		}
		if delivered == 0 {
			logger.Warn("federation 公告失败(心跳兜底重推)", zap.String("hash", shortHash(hash)))
			return
		}
		s.mu.Lock()
		if e := s.entries[hash]; e != nil {
			e.confirmed = true
		}
		s.mu.Unlock()
	})
}

// ShareDeleted share 生命周期钩子：撤销公告（best-effort，索引立即移除；
// 撤销失败时 registry 条目按公告 TTL 自动过期）。
func (s *Service) ShareDeleted(code string) {
	hash := codeHash(code)
	s.mu.Lock()
	delete(s.entries, hash)
	s.mu.Unlock()

	// 异步撤销同 ShareCreated：best-effort 协程无监督者，GoSafe 兜底 panic 面
	//（撤销失败时 registry 条目按公告 TTL 自动过期，不影响正确性）。
	async.GoSafe(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, cl := range s.clients {
			if err := cl.revoke(ctx, s.nodeID, s.priv, hash); err != nil {
				logger.Debug("federation 撤销失败(条目 TTL 自动过期)", zap.Error(err))
			}
		}
	})
}

// ResolveInfo 解析结果：口令在联邦内的源节点。
type ResolveInfo struct {
	NodeID    string    `json:"node_id"`
	URL       string    `json:"url"`
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expires_at"`
	SizeHint  int64     `json:"size_hint"`
}

// Resolve 转查 registry（读路径依次主→备,首个命中即返回）。
// 全部未接入返回 (nil, nil)——调用方（resolve 代理 handler）以 available:false
// 回给前端，不区分"未启用/不存在"（降探测面）。
func (s *Service) Resolve(code string) (*ResolveInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, cl := range s.clients {
		info, err := cl.resolve(ctx, code)
		if err != nil {
			continue // 单 registry 故障不阻断,查下一家
		}
		if info != nil {
			return info, nil
		}
	}
	return nil, nil
}

// Stop 停止心跳循环并注销节点（best-effort；测试与优雅停机用）。
func (s *Service) Stop() {
	s.stopped.Do(func() { close(s.stop) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, cl := range s.clients {
		if err := cl.deregister(ctx, s.nodeID, s.priv); err != nil {
			logger.Debug("federation 注销失败(租约 TTL 自动过期)", zap.Error(err))
		}
	}
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}
