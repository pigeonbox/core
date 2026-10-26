// Package anonymous 实现匿名取件 service（仿 vastsa/PigeonBox UX）。
//
// 核心流程：
//  1. 上传方：上传文件后，调用 GenerateCode 获取 6 位取件码（建立 pickup_code → share_code 映射）
//  2. 取件方：输入 6 位码 + 可选密码，按码取文件
//  3. 系统：校验（DB 为准）→ 扣减次数（DB 原子）→ 返回下载信息
//
// 设计要点（DB 为唯一真相源）：
//   - 6 位取件码落 file_codes.pickup_code 列（真相源，唯一索引防撞码）；
//     Redis 仅存 pickup_code → share_code 映射 + 展示信息作为加速缓存
//     （TTL=分享过期；永久分享不写 KV，经 DB 回退路径解析，重启不失联）
//   - 过期时间、剩余次数、密码哈希全部以 file_codes 表为准
//   - 取件码字符表去掉易混淆字符 0/O/1/I/L
package anonymous

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oekit/concurrency/singleflight"
	"github.com/oekit/kitx/uidgen"
	"github.com/pigeonbox/core/conf"
	pkgerrors "github.com/pigeonbox/core/pkg/errors"
	"github.com/pigeonbox/core/pkg/gate"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// 6 位取件码字符表（去掉易混淆字符 0/O/1/I/L）
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
const codeLength = model.PickupCodeLength // 唯一真相源在 model

// shareCode 分享码长度（file_codes.code，字母数字混合、区分大小写）
const shareCodeLength = 8

// Redis key 模板（仅存映射 + 展示信息，真实状态查 DB）
const (
	keyPickupCodeMapping = "anon:code:%s" // pickup_code -> share_code
	keyPickupCodeNeg     = "anon:neg:%s"  // 负缓存标记：KV 未命中且 DB 也无此码（防枚举穿透打库）
)

// 回源缓存的 TTL：
//   - 负缓存短（2min）：新分享建好后最多遮蔽 2min（仅影响"建前被探测过同名的
//     自定义码"这一罕见时序，正常取件码走映射键不受影响），自愈。
//   - 回源回填短（5min）：DB 是真相源，回填只是缓存加速；TTL 到期自动回落直查。
const (
	negCacheTTL   = 2 * time.Minute
	dbBackfillTTL = 5 * time.Minute
)

// 错误哨兵
var (
	ErrCodeNotFound  = errors.New("pickup code not found")
	ErrCodeExpired   = errors.New("pickup code expired")
	ErrCodeExhausted = errors.New("pickup code exhausted")
	ErrPasswordWrong = errors.New("password wrong")
	ErrNotReady      = errors.New("share upload incomplete") // 登记占位未回填物理文件
)

// BlockedError 分享处于管控拒绝态（治理状态机）。
// handler 侧 errors.As 后按 ErrCode 透传（20012 blocked / 20013 pending_review）。
// 实现收口 pkg/errors.ShareBlocked——历史上 share.ShareBlockedError 持有一份
// 逐字相同的拷贝（本域还用字面量而非常量，已漂移），2026-10-10 合并；
// 别名保持域内构造点与 handler 用法不变。
type BlockedError = pkgerrors.ShareBlocked

// redisKV 匿名取件所需的最小 Redis 命令集（*redis.Client 天然满足；
// 字段收窄为方法集以便注入内存 mock，构造函数仍收具体客户端）。
type redisKV interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	SetArgs(ctx context.Context, key string, value any, a redis.SetArgs) *redis.StatusCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

// Service 匿名取件 service。
// Redis 仅存映射 + 展示信息；过期/次数/密码等真实状态全部以 file_codes 表为准。
type Service struct {
	rdb          redisKV
	fileCodeRepo *dao.FileCodeRepository
	// flights 同码并发回源合并（oekit singleflight，零值安全）：
	// KV 双未命中到负标记落定之间是击穿窗口，枚举/重试风暴下 N 个相同码
	// 会放大成 N 轮 DB 查询，Flight 合并为 1 轮（见 lookupShareCode）。
	flights *singleflight.Flight[string]
}

// NewService 创建 service（repo 构造注入，去全局化终态）。
// rdb nil 归一化：typed-nil 接口会骗过"Redis 可用"守卫（!= nil 判真）。
// rdb 为 nil（redis.host 未配置）时进入单机内存模式：映射/展示信息存进程内
// TTL KV，匿名取件全功能可用（单进程语义等价；重启丢失、不跨副本共享）。
func NewService(rdb *redis.Client, gormDB *gorm.DB) *Service {
	fileCodeRepo := dao.NewFileCodeRepository(gormDB)
	var kv redisKV
	if rdb != nil {
		kv = rdb
	} else {
		kv = newMemoryKV()
	}
	return &Service{rdb: kv, fileCodeRepo: fileCodeRepo, flights: singleflight.New[string]()}
}

// CodeMeta 取件码展示信息（仅存于 Redis，真实状态查 DB）
type CodeMeta struct {
	ShareCode   string // 真实 file_code
	FileName    string
	FileSize    int64
	RequireAuth bool // 仅展示"是否需要密码"
	// ExpireAt 分享过期时间（Unix 秒；0=未设/长期）。Retrieve 成功回填，
	// 供取件结果页展示（此前 handler 恒发 0，页面"过期时间 -"）。
	ExpireAt int64
	// RemainingCount 本次取件后剩余可取次数：-1=不限次，>0=剩余。
	// 与 model.FileCode.ExpiredCount 语义一致（0=耗尽不会出现在成功响应里）。
	RemainingCount int
	// IsText 文本分享标记（Retrieve 时按 DB 回填，不进 KV meta 串）。
	// 文本分享取件后前端跳详情页展示（结果页无文本渲染形态），handler 据此回传 share_code。
	IsText bool
}

// GenerateCode 生成 6 位取件码：落库 file_codes.pickup_code（真相源，唯一索引
// 防跨分享撞码），有 TTL 时另写 KV 映射加速解析。expireAt 零值=永久分享（不写
// KV，取件走 DB 回退）；过期时间应与 DB 记录对齐，已过期则拒绝铸造。
func (s *Service) GenerateCode(ctx context.Context, meta CodeMeta, expireAt time.Time) (string, error) {
	if s.fileCodeRepo == nil {
		return "", errors.New("fileCodeRepo 未注入，取件码无法落库")
	}
	ttl := time.Until(expireAt)
	if !expireAt.IsZero() && ttl <= 0 {
		return "", errors.New("expireAt 已过期")
	}
	for i := 0; i < 10; i++ {
		code := randomCode()
		// 跨列避让（2026-10-23）：取件码若与某个**自定义分享码**撞同一字符串，
		// 分享域「按码取分享」按 pkg/codex 的优先级（**取件码优先**）⇒ 会命中
		// 取件码那条 ⇒ 自定义码那条打不开（错配）。⇒ 铸造时确认该串在 code 列
		// 也未被占用（占用则换码重试）。
		//
		// ⚠️ 必须用**折叠**检查（CheckCodeExistsFolded）：分享域解析在精确未命中后
		// 还有 UPPER 兜底（大小写不敏感的自定义码）⇒ 只做精确检查会漏掉
		// 「code 列存小写 ws9whp、铸造出大写 WS9WHP」这种组合 ⇒ 两条分享并存，
		// 而按大写码解析会命中那条小写自定义码 ⇒ 取件码打不开自己的分享、
		// 反而打开别人的。
		if taken, cerr := s.fileCodeRepo.CheckCodeExistsFolded(ctx, code, 0); cerr == nil && taken {
			continue
		}
		// 真相源落库（唯一索引 + 占用预查防跨分享撞码）；占用则换码重试
		set, err := s.fileCodeRepo.SetPickupCode(ctx, meta.ShareCode, code)
		if err != nil {
			return "", err
		}
		if !set {
			continue
		}
		// KV 映射是加速缓存：永久分享（零值 expireAt，无 TTL 可言）或 rdb 缺席时
		// 跳过，取件经 lookupShareCode 的 DB 回退路径解析
		if s.rdb == nil || ttl <= 0 {
			return code, nil
		}
		// SetNX 已废弃（SA1019），改用 Set + NX 选项；NX 且键已存在时返回 redis.Nil
		key := fmt.Sprintf(keyPickupCodeMapping, code)
		_, err = s.rdb.SetArgs(ctx, key, meta.ShareCode, redis.SetArgs{TTL: ttl, Mode: "NX"}).Result()
		if err != nil {
			// KV 撞码（残留脏数据）或故障：回滚 DB 列后换码重试/报错，
			// 防"DB 已落但 KV 指向他处"的错位映射
			_ = s.fileCodeRepo.ClearPickupCode(ctx, code)
			if errors.Is(err, redis.Nil) {
				continue
			}
			return "", err
		}
		return code, nil
	}
	return "", errors.New("failed to generate unique code after 10 retries")
}

// lookupShareCode 把用户输入解析为分享码：
// 6 位输入按取件码处理（容忍小写，规范化后查映射）；KV 命中即返回，
// 未命中走 resolveShareCodeFromDB 回源（singleflight 同码合并，见上）。
func (s *Service) lookupShareCode(ctx context.Context, code string) (string, error) {
	trimmed := strings.TrimSpace(code)
	if len(trimmed) == codeLength {
		trimmed = strings.ToUpper(trimmed)
	}
	mappingKey := fmt.Sprintf(keyPickupCodeMapping, trimmed)
	shareCode, err := s.rdb.Get(ctx, mappingKey).Result()
	if err == nil {
		return shareCode, nil
	}
	if !errors.Is(err, redis.Nil) {
		return "", err
	}
	// 负缓存快速路径：近期已确认"映射与 DB 均无此码"
	if _, err := s.rdb.Get(ctx, fmt.Sprintf(keyPickupCodeNeg, trimmed)).Result(); err == nil {
		return "", ErrCodeNotFound
	}
	// 同码并发回源合并：waiter 各自按自身 ctx 等待/取消（互不连坐）；
	// DB 瞬时错误会共享给等待方，但错误路径不落任何缓存，下一请求自然重试
	shareCode, err = s.flights.Do(ctx, "anon:lookup:"+trimmed, func(ctx context.Context) (string, error) {
		return s.resolveShareCodeFromDB(ctx, trimmed, mappingKey)
	})
	if err != nil {
		return "", err
	}
	return shareCode, nil
}

// resolveShareCodeFromDB KV 未命中后的 DB 回源，带双向缓存：
// ① pickup_code 列（真相源：KV 重启丢失/永久分享未写 KV 时由此解析）
// ② code 列直查（8 位分享码/自定义口令直输，折叠开启时 UPPER 兜底）。
// 命中回填映射（短 TTL，重复查询不再打库）；DB 也无此码则放负缓存标记
// （短 TTL，防取件码枚举穿透打库）。
// 真实状态（过期/次数/密码）始终以 DB 为准，缓存仅加速"码→分享码"解析。
func (s *Service) resolveShareCodeFromDB(ctx context.Context, trimmed, mappingKey string) (string, error) {
	// DB 回源（2026-10-23 收敛）：走统一解析器（dao.ResolveByCode）—— 取件码优先、
	// 分享码精确 → 大小写折叠。此前本域与分享域/MCP 各写一套（优先级与折叠
	// 口径不一致）⇒ 同一串码在不同端点可能解析到不同分享。
	fc, dbErr := s.fileCodeRepo.ResolveByCode(ctx, trimmed, conf.CodeFoldEnabledOrDefault())
	if dbErr != nil || fc == nil {
		// 回源 DB 也没有：放负缓存标记（防取件码枚举穿透打库）
		s.rdb.Set(ctx, fmt.Sprintf(keyPickupCodeNeg, trimmed), "1", negCacheTTL)
		return "", ErrCodeNotFound
	}
	// 回源命中：回填映射缓存（真实状态仍查 DB）
	s.rdb.Set(ctx, mappingKey, fc.Code, dbBackfillTTL)
	return fc.Code, nil
}

// enrichMetaFromDB 用 DB 记录补齐展示信息（真相源 = DB）。
//
// 2026-10-25：Redis 的「展示信息快照」(anon:meta:*) 已删 ⇒ 展示名/大小一律从 DB 现算：
//
//	· 文件分享：DisplayName()（原始名(Text) → 存储名(UUIDFileName) → 路径末段）+ Size；
//	  （2026-10-26 订正：旧注释写的顺序与实现**相反**）
//	· 文本分享：没有文件名 ⇒ 展示名取**文本预览**、大小取**文本字节数**
//	  （与铸造取件码时的口径一致 ⇒ 取件页显示与铸造时相同）。
func enrichMetaFromDB(meta *CodeMeta, fc *model.FileCode) {
	if meta.FileName == "" {
		if fc.IsTextShare() {
			meta.FileName = utils.TextPreview(fc.Text)
		} else {
			meta.FileName = fc.DisplayName()
		}
	}
	if meta.FileSize == 0 {
		if fc.IsTextShare() {
			meta.FileSize = int64(len(fc.Text))
		} else {
			meta.FileSize = fc.Size
		}
	}
	meta.RequireAuth = fc.RequireAuth
}

// Retrieve 按取件码取件（校验 + 扣减次数，DB 为准）。
// 返回展示信息 CodeMeta。每次成功调用扣减一次剩余次数。
// clientIP 为可信代理解析后的取件方 IP（handler 注入，防爆破锁定键维度）。
func (s *Service) Retrieve(ctx context.Context, code, password, clientIP string) (*CodeMeta, error) {
	if s.rdb == nil {
		return nil, errors.New("redis 未配置，匿名取件功能不可用")
	}
	// 1. 解析取件码/分享码 → share_code
	shareCode, err := s.lookupShareCode(ctx, code)
	if err != nil {
		return nil, err
	}

	// 2. 展示信息：真相源是 DB（下方 enrichMetaFromDB 回填）——
	//    此前另有一份 Redis「展示信息快照」(anon:meta:*)，字段与 DB 重复且
	//    content_type 无 DB 来源（恒为 application/octet-stream）⇒ 已删（2026-10-25
	//    删除旧版本兼容）；Redis 只留「取件码→分享码」映射（热路径）与负缓存。
	meta := &CodeMeta{ShareCode: shareCode}

	// 3. 查 DB 真实状态
	fc, err := s.fileCodeRepo.GetByCode(ctx, shareCode)
	if err != nil {
		return nil, ErrCodeNotFound
	}
	enrichMetaFromDB(meta, fc)

	// 4. 统一取件闸门（pkg/gate.CheckPickup）：过期（时间）→ 管控
	// （blocked/pending_review）→ 未完成登记（FilePath 空且非文本，此前会先扣
	// 次数再在下载时 500）→ 密码 + 防爆破锁定。锁定与 select/download/preview
	// 面共享 scope "pickup" 的 (IP, code) 失败计数——此前本域锁定在 handler 层
	// 且独立 scope "anon"，同一口令跨入口爆破预算翻倍。
	//
	// ⚠️ 次数耗尽与时间过期**分开判定**（2026-10-24）：此前两者都落在 IsExpired
	// 里 ⇒ 匿名通道在 retrieve 里把次数扣到 0 后，随后的下载被判「已过期」⇒
	// **按取件次数的分享，最后一次取件必然拿不到文件**（分享页通道却能拿到）。
	// 现在 retrieve 只**检查**不扣减（扣次统一在内容交付时），次数耗尽给独立
	// 错误码 20007「取件码已用完」。
	if fc.IsCountExhausted() {
		return nil, ErrCodeExhausted
	}
	decision := gate.CheckPickup(ctx, gate.PickupState{
		Expired:       fc.IsExpired(),
		Blocked:       fc.IsBlockedShare(),
		BlockedStatus: fc.Status,
		NotReady:      !fc.IsTextShare() && fc.GetFilePath() == "",
		RequireAuth:   fc.RequireAuth,
		PasswordHash:  fc.PasswordHash,
	}, gate.PickupCheck{
		ClientIP: clientIP,
		// 失败记账用**解析后的分享码**（shareCode）而不是用户给的码 ⇒ 同一分享
		// 的 6 位取件码与 8 位分享码共享一把钥匙，爆破预算不翻倍（2026-10-23 审计）。
		Code:                 shareCode,
		Password:             password,
		MissingCountsFailure: true, // 保持本域历史规格：空密码按失败记账
	})
	switch decision.Verdict {
	case gate.PickupUnavailable:
		return nil, ErrCodeExpired
	case gate.PickupBlocked:
		return nil, &BlockedError{Status: decision.BlockedStatus}
	case gate.PickupNotReady:
		return nil, ErrNotReady
	case gate.PickupLocked:
		return nil, &middleware.LockedError{RemainingSeconds: decision.LockRemain}
	case gate.PickupPasswordMissing, gate.PickupPasswordWrong:
		return nil, ErrPasswordWrong
	}

	// 6. 取件次数：**只检查、不扣减**（2026-10-24）。
	//
	// 扣次统一发生在**内容交付时**（/share/download 的 chargePickup，按取件会话
	// /下载令牌计次）——此前 retrieve 自身扣 1 次、随它下发的直链下载又扣 1 次
	// ⇒ 一次取件扣两次（count 样式下对方可用次数直接减半）；且「扣到 0 ⇒
	// IsExpired ⇒ 判过期」使**最后一次取件必然拿不到文件**。
	// 现在 retrieve 只负责「发凭证」，次数在交付那一刻扣，且同一凭证只扣一次。
	//
	// 剩余次数回填：本次**未**扣减 ⇒ 剩余 = 当前剩余（-1=不限）。
	// 过期时间回填（结果页展示；此前 handler 恒发 0 ⇒ 页面显示"过期时间 -"）。
	if fc.ExpiredAt != nil {
		meta.ExpireAt = fc.ExpiredAt.Unix()
	}
	meta.RemainingCount = fc.ExpiredCount
	if fc.ExpiredCount > 0 {
		// 语义沿用「本次取件后剩余可取次数」（CodeMeta 注释）：本次取件对应的
		// 那一次交付会扣掉 1 ⇒ 结果页展示 N-1。
		meta.RemainingCount = fc.ExpiredCount - 1
	}
	meta.IsText = fc.IsTextShare()

	return meta, nil
}

// Cancel 作废取件码（KV 映射与 DB pickup_code 列同步清除，真相源一致）
func (s *Service) Cancel(ctx context.Context, code string) error {
	if s.fileCodeRepo != nil {
		if err := s.fileCodeRepo.ClearPickupCode(ctx, strings.ToUpper(strings.TrimSpace(code))); err != nil {
			return err
		}
	}
	return s.cleanup(ctx, code)
}

// Peek 按取件码查询分享信息（不扣次数、不校验密码），仅供展示。
// 返回展示信息 CodeMeta + DB 记录（含剩余次数、过期时间等）。
func (s *Service) Peek(ctx context.Context, code string) (*CodeMeta, *model.FileCode, error) {
	if s.rdb == nil {
		return nil, nil, errors.New("redis 未配置，匿名取件功能不可用")
	}
	shareCode, err := s.lookupShareCode(ctx, code)
	if err != nil {
		return nil, nil, err
	}
	meta := &CodeMeta{ShareCode: shareCode} // 展示信息以 DB 为准（enrichMetaFromDB）
	fc, err := s.fileCodeRepo.GetByCode(ctx, shareCode)
	if err != nil {
		return nil, nil, ErrCodeNotFound
	}
	enrichMetaFromDB(meta, fc)
	// 过期分享与不存在同语义（统一 404 防探测），与 /share/metadata、Retrieve
	// 对齐——此前仅查封禁不查过期，过期分享的元数据经本端点永久可查
	// （v0.11.1 修复）。
	//
	// ⚠️ **次数耗尽不在本层拦截**（2026-10-24）：Peek 是「查得到但已失效」的
	// 元信息面，把耗尽态在这里吞成 ErrCodeNotFound ⇒ 调用方（/anonymous/download
	// 与 /anonymous/search）拿到的就是「码不存在」⇒ 20007「取件码已用完」这条
	// 独立错误码永远发射不出来，「次数耗尽」又退回了「不存在/已过期」的伪装。
	// ⇒ 本层只判**时间**过期；次数耗尽由调用方按自己的口径判定
	// （/anonymous/download → 20007；/anonymous/search → 20007）。
	if fc.IsExpired() {
		return nil, nil, ErrCodeNotFound
	}
	if fc.IsBlockedShare() {
		return nil, nil, &BlockedError{Status: fc.Status}
	}
	return meta, fc, nil
}

// ============ 内部辅助 ============

// randomCode 生成 6 位随机码（crypto/rand 经 utils 统一收口，字符表见 codeAlphabet）
func randomCode() string {
	return uidgen.RandomString(codeAlphabet, codeLength)
}

func (s *Service) cleanup(ctx context.Context, code string) error {
	return s.rdb.Del(ctx, fmt.Sprintf(keyPickupCodeMapping, code)).Err()
}

// AnonymousShareParams 匿名分享端到端参数
type AnonymousShareParams struct {
	FilePath       string
	FileName       string
	FileSize       int64
	ExpireAt       *time.Time // 必填，决定 DB 过期时间 + Redis TTL
	MaxPickupCount int        // -1=无限, 0=默认无限, >0=限制
	Password       string     // 空=无密码
}

// CreateAnonymousShare 端到端：建 file_codes 记录 + 生成取件码。
// 供 gen handler 调用，避免 handler 直接操作 DAO；密码在此 bcrypt 哈希后存 DB。
// 返回 6 位取件码。
func (s *Service) CreateAnonymousShare(ctx context.Context, p AnonymousShareParams) (string, error) {
	if p.ExpireAt == nil {
		return "", errors.New("ExpireAt 必填")
	}
	maxCount := p.MaxPickupCount
	if maxCount == 0 {
		maxCount = -1 // 默认无限
	}

	hash, err := utils.HashPassword(p.Password)
	if err != nil {
		return "", err
	}

	fc := &model.FileCode{
		Code:         randomShareCode(),
		FilePath:     p.FilePath,
		UUIDFileName: p.FileName,
		Size:         p.FileSize,
		ExpiredAt:    p.ExpireAt,
		ExpiredCount: maxCount,
		RequireAuth:  p.Password != "",
		PasswordHash: hash,
		UploadType:   "anonymous",
	}
	if err := s.fileCodeRepo.Create(ctx, fc); err != nil {
		return "", err
	}

	pickupCode, err := s.GenerateCode(ctx, CodeMeta{
		ShareCode:   fc.Code,
		FileName:    p.FileName,
		FileSize:    p.FileSize,
		RequireAuth: p.Password != "",
	}, *p.ExpireAt)
	if err != nil {
		// 回滚 DB 记录（物理文件未落库，无需清）
		if dErr := s.fileCodeRepo.Delete(ctx, fc.ID); dErr != nil {
			logger.CtxWarn(ctx, "rollback file_code on generate code failed", "id", fc.ID, "err", dErr)
		}
		return "", err
	}
	return pickupCode, nil
}

// MintForShare 为既有分享铸造 6 位取件码（2026-10-08 起文本/文件/永久分享同权，
// "只保留 6 位码"呈现；E2E 密文分享由调用方跳过——密钥只随链接，凭码取不到明文）。
// 真相源落 DB pickup_code 列；有 TTL 时另写 KV 映射加速解析：
//   - expireAt nil（永久分享）→ 零值传入 GenerateCode：不写 KV（无 TTL 可言），
//     取件经 lookupShareCode 的 pickup_code 列回退路径解析，重启不失联
//   - KV 写失败返回错误但调用方应视为非致命（出码主流程不阻断）
func (s *Service) MintForShare(ctx context.Context, shareCode, fileName string, fileSize int64, requireAuth bool, expireAt *time.Time) (string, error) {
	if s.rdb == nil {
		return "", nil
	}
	if expireAt != nil && time.Until(*expireAt) <= 0 {
		return "", nil
	}
	if expireAt == nil {
		return s.GenerateCode(ctx, CodeMeta{
			ShareCode:   shareCode,
			FileName:    fileName,
			FileSize:    fileSize,
			RequireAuth: requireAuth,
		}, time.Time{})
	}
	return s.GenerateCode(ctx, CodeMeta{
		ShareCode:   shareCode,
		FileName:    fileName,
		FileSize:    fileSize,
		RequireAuth: requireAuth,
	}, *expireAt)
}

// ResolvePlaceholder 校验取件码对应一个"待回填"的占位分享（/anonymous/generate
// 创建、FilePath 为空），返回其 8 位分享码。供 presign 直传绑定通道在 Init 时
// 预检、Complete 时定位回填目标。
// 非占位（已完成上传）/不存在/已过期 → 显式错误，防把对象绑到任意分享上。
func (s *Service) ResolvePlaceholder(ctx context.Context, pickupCode string) (string, error) {
	if s.rdb == nil {
		return "", errors.New("redis 未配置，匿名取件功能不可用")
	}
	shareCode, err := s.lookupShareCode(ctx, pickupCode)
	if err != nil {
		return "", ErrCodeNotFound
	}
	fc, err := s.fileCodeRepo.GetByCode(ctx, shareCode)
	if err != nil || fc == nil {
		return "", ErrCodeNotFound
	}
	if fc.IsTextShare() || fc.GetFilePath() != "" {
		return "", errors.New("该取件码已完成上传，不能重复绑定")
	}
	if fc.IsExpired() {
		return "", ErrCodeExpired
	}
	return shareCode, nil
}

// BindFilePath 把直传完成的对象回填到占位分享（匿名取件码直传通道的最后一公里）。
// 以实际落盘对象为准更新 file_path 与 size；完成标记由调用方（presign Complete）保证。
// note 非空时一并写入（先出码会话随 init 携带的文件备注）。
//
// ⚠️ 2026-10-26 审计修复：**原始文件名曾被永久丢弃**。
// 占位记录把 generate 时的**原始名**寄存在 `uuid_file_name`（因为占位期
// file_path 为空 ⇒ 不能写 text：text 非空会让 IsTextShare() 把「待上传的
// 文件分享」误判成**文本分享**）；而绑定时又把 uuid_file_name 清空 ⇒
// 原始名彻底消失 ⇒ 展示名退化成存储名（up_<uuid>.pdf）——取件页/管理台/
// 用户列表/MCP/传输日志**全部**显示 UUID。
// ⇒ 绑定时把原始名**转写进 text**（此时 file_path 已非空 ⇒ 不再误判文本分享），
// `uuid_file_name` 回归本义（存储名 = 路径末段），一列只担一种语义。
func (s *Service) BindFilePath(ctx context.Context, shareCode, filePath string, size int64, note string) error {
	fc, err := s.fileCodeRepo.GetByCode(ctx, shareCode)
	if err != nil || fc == nil {
		return ErrCodeNotFound
	}
	cols := map[string]interface{}{
		"file_path":      filePath,
		"uuid_file_name": storageBaseName(filePath),
		"size":           size,
	}
	// 原始名转写进 text（占位期寄存在 uuid_file_name；text 为空才写，避免覆盖）
	if strings.TrimSpace(fc.Text) == "" {
		if original := strings.TrimSpace(fc.UUIDFileName); original != "" {
			cols["text"] = original
		}
	}
	if note != "" {
		cols["note"] = note
	}
	return s.fileCodeRepo.UpdateColumns(ctx, fc.ID, cols)
}

// storageBaseName 取存储相对路径的末段（存储名；无路径时返回空串）。
func storageBaseName(rel string) string {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return ""
	}
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[i+1:]
	}
	return rel
}

// randomShareCode 8 位 file_code（crypto/rand，小写字母+数字）
func randomShareCode() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	return uidgen.RandomString(charset, 8)
}
