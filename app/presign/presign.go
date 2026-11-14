// Package presign 实现预签名上传 service。
//
// 流程：
//  1. 客户端调 Init 申请预签名 URL + token
//  2. 客户端 PUT 直传文件到 URL
//  3. 客户端调 Complete 通知服务写 share 表
//  4. 服务返回 share_code
//
// 当前实现：fallback 走"自家直传 URL + token"模式（不依赖真实 S3/OSS 预签名）。
// 当 OpenDAL binding 接入后，可直接生成真实预签名 URL。
package presign

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oekit/kitx/uidgen"
	"github.com/redis/go-redis/v9"

	"github.com/pigeonbox/core/internal/uploadlock"
	"github.com/pigeonbox/core/pkg/baseurl"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/dao"
	"gorm.io/gorm"
)

// Redis key 模板
const (
	keyUploadMeta = "presign:meta:%s" // upload_id -> InitMeta (JSON)

	// transferWindow 在途传输保护窗口：会话记录的存活期须覆盖整条传输 +
	// 随后的 Complete，不得被签名时效中途清掉。
	//
	// 2026-11-03 真机实录：2.5GB .img 慢速上传，PUT 跑了 28.6 分钟，而会话时效
	// （download.presign_expire_seconds，线上=600s）在传输途中把 meta 键 TTL 跑
	// 完 ⇒ 键被 Redis 静默清掉；PUT 只在**开头**查过期 ⇒ 流照常传完并返回 200，
	// 27ms 后 Complete 却报 10004「not found」（连"过期"都报不出来）⇒ 2.5GB
	// 对象成孤儿。语义修正：签名时效只约束「PUT 发起时刻」，传输过程由 HTTP 层
	// 超时约束，不再受签名时效摆布。
	//
	// self 方案：传输期间键允许过期（PUT 结束的回写会重建，见 UploadDirect
	// 第 5 步）；s3 方案传输不经本进程、无从回写 ⇒ Init 就按本窗口存 meta。
	transferWindow = 2 * time.Hour

	// completeGrace 数据面传完后的 Complete 宽限：PUT 成功后 meta 以此时长
	// 回写（含 Transferred/FileHash），客户端拿什么速度收尾都能 Complete。
	completeGrace = 30 * time.Minute
)

// 错误定义
var (
	ErrUploadNotFound  = errors.New("upload not found")
	ErrTokenInvalid    = errors.New("upload token invalid")
	ErrUploadExpired   = errors.New("upload expired")
	ErrAlreadyComplete = errors.New("upload already completed")
)

// redisKV presign 会话所需的最小 Redis 命令集（*redis.Client 天然满足；
// 字段收窄为方法集以便注入内存 mock，构造函数仍收具体客户端）。
type redisKV interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

// Service 预签名上传 service
type Service struct {
	rdb           redisKV
	defaultExpire time.Duration
	signingKey    []byte
	baseURL       string
	// baseURLProvider 对外 base 地址的动态来源（2026-10-22 站点公网地址）。
	// 直传的 upload-direct 回跳地址也走它——与管理后台填的公网地址同源，
	// 否则直传链接会带内网地址，外部浏览器打不开。
	baseURLProvider func(context.Context) string
	// shareService 注入的 share service（Complete 时调用写分享表）
	shareService ShareServiceInterface
	// pickupBinder 匿名取件码占位绑定（bootstrap 注入 anonymous service；nil = 绑定通道不可用）
	pickupBinder PickupBinder
	// storage 注入的存储服务（直传按当前激活后端落盘；nil 时保留本地盘直写）
	storage StorageWriter
	// objects 真预签名直传能力（s3 后端可用；nil 或不支持时回退自家中转）
	objects ObjectStore
	// fileCodeRepo 秒传检索用（此前 CheckQuickUpload 方法体内联现建）
	fileCodeRepo *dao.FileCodeRepository
	// nowFn 过期判定的时钟注入点（缺省真实墙钟；测试可控推进——签名时效的
	// 生效与否是「传输期跨越过期时刻」类缺陷的判据量，必须可被测试测量）。
	nowFn func() time.Time
}

// nowTime 会话过期判定的统一时间源。
func (s *Service) nowTime() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

// 直传模式标记（meta.Scheme / InitResult.Scheme，服务端判定为准）
const (
	// SchemeSelf 自家中转：客户端 PUT /api/v1/presign/upload-direct/:uploadID
	SchemeSelf = "self"
	// SchemeS3 真·预签名直传：客户端 PUT 对象存储，服务器只签名不过流量
	SchemeS3 = "s3"
)

// StorageWriter 存储写入抽象（由 *storage.StorageService 实现）。
// 单方法接口避免反向依赖存储包，也保持既有 StorageInterface/mock 不动。
type StorageWriter interface {
	SaveBytes(ctx context.Context, savePath string, data []byte) error
	DeleteFile(ctx context.Context, filePath string) error
}

// streamWriter 流式写入能力（*storage.StorageService 实现；storage 包的
// StorageInterface 自身已含 SaveStream）。包内私有能力探测：有则流式写，
// 无则退化为缓冲写——避免为类型断言具体实现而 import 存储包。
type streamWriter interface {
	SaveStream(ctx context.Context, savePath string, r io.Reader, expectedSize int64) (int64, error)
}

// headReader 魔数复检读取能力（*storage.StorageService 实现；与 streamWriter
// 同款能力探测模式，Complete 阶段读对象头部 512B 做内容检查）。
type headReader interface {
	GetFileReader(ctx context.Context, filePath string) (io.ReadCloser, int64, error)
}

// ObjectStore 真预签名直传能力（由 *storage.StorageService 实现）。
// PresignPutURL 对 local/webdav 返回 storage.ErrPresignUnsupported，调用方回退自家中转。
type ObjectStore interface {
	PresignPutURL(ctx context.Context, objectKey string, expire time.Duration) (string, error)
	// HeadObject 直传完成核实用：对象必须真实存在并取实际大小（S3 为事实源）
	HeadObject(ctx context.Context, objectKey string) (size int64, etag string, err error)
}

// PickupBinder 匿名取件码占位绑定窄接口（app/anonymous.Service 结构性满足；
// presign 域不 import anonymous 域，由 bootstrap 注入）。
type PickupBinder interface {
	// ResolvePlaceholder 校验取件码是待回填占位分享，返回其 8 位分享码
	ResolvePlaceholder(ctx context.Context, pickupCode string) (string, error)
	// BindFilePath 把直传对象回填到占位分享（file_path+size；note 非空一并写入）
	BindFilePath(ctx context.Context, shareCode, filePath string, size int64, note string) error
}

// ShareCreateReq 经 share 域建分享的窄化请求（消费侧词汇表）。
// 此前本域直接 import share.ShareFileReq/ShareResp 构成唯一约定 app 跨域边
// （presign→share），2026-10-10 消解：字段与 share.CreateShare 对齐，
// bootstrap 适配器（presignShareAdapter）转换到 share 域类型——与
// requestShareGateway/mcpShareAdapter 同款装配点转换模式。
type ShareCreateReq struct {
	Channel      string // 上传通道（metrics 用；presign 恒 "presign"）
	FilePath     string
	Size         int64
	Text         string
	ExpiredAt    *time.Time
	ExpiredCount int
	RequireAuth  bool
	PasswordHash string
	UserID       *uint
	UploadType   string
	OwnerIP      string
	FileHash     string
	UploadID     string
	Note         string // 文件备注（可空，取件页展示；2026-10-12 #489）
}

// ShareCreateResp share 域建分享结果的窄视图（presign 消费字段）。
type ShareCreateResp struct {
	Code         string
	ShareURL     string
	FullShareURL string
	PickupCode   string
}

// ShareServiceInterface share service 接口（消费侧窄接口，bootstrap 注入
// share 域适配器；避免 app 跨域 import）。
type ShareServiceInterface interface {
	CreateShare(ctx context.Context, req *ShareCreateReq) (*ShareCreateResp, error)
	// FindShareByUploadIDOwnedBy 按上传会话 ID 回读**属于调用方**的已建分享的
	// 分享码（上传完成面的**幂等键**：同一 upload_id 二次 Complete 必须复用
	// 同一个分享，而不是再建一条 ⇒ 否则 owner 的「最多取 N 次」额度翻倍。
	// 2026-10-24 审计，core v0.32.6）。
	//
	// ⚠️ 必须带属主判据（2026-11-06 P0）：upload_id 由客户端自报的 file_hash
	// 派生 ⇒ 他人可算出 ⇒ 无限定的回读等于「谁猜到 upload_id 谁拿走别人的分享码」。
	// 属主不符按未命中处理（返回 "", nil），调用方随后建自己的分享。
	// ⇒ 原无限定的 FindShareByUploadID 已从本接口删除，避免调用点再次选错。
	//
	// 只回分享码（base URL 由消费侧自算）⇒ 窄接口保持窄。
	FindShareByUploadIDOwnedBy(ctx context.Context, uploadID string, userID *uint, ownerIP string) (string, error)
}

// NewService 创建 service。rdb nil 归一化：typed-nil 接口会骗过
// "Redis 可用"守卫（!= nil 判真）。
// rdb 为 nil（redis.host 未配置）时进入单机内存模式：直传会话存进程内
// TTL KV，预签名直传全功能可用（单进程语义等价；重启未完成会话作废）。
func NewService(gormDB *gorm.DB, rdb *redis.Client, baseURL string, signingKey string) *Service {
	var kv redisKV
	if rdb != nil {
		kv = rdb
	} else {
		kv = newMemoryKV()
	}
	return &Service{
		rdb:           kv,
		defaultExpire: 1 * time.Hour,
		signingKey:    []byte(signingKey),
		baseURL:       baseURL,
		fileCodeRepo:  dao.NewFileCodeRepository(gormDB),
	}
}

// SetBaseURLProvider 注入对外 base 地址的动态来源（2026-10-22 站点公网地址）。
// 与 share 域同源：管理后台「站点公网地址」一处改，链接与直传回跳地址同时生效。
func (s *Service) SetBaseURLProvider(fn func(context.Context) string) {
	s.baseURLProvider = fn
}

// resolveBaseURL 取本次请求该用的对外 base（provider 优先，其次构造期值）。
func (s *Service) resolveBaseURL(ctx context.Context) string {
	if s.baseURLProvider != nil {
		if v := strings.TrimSpace(s.baseURLProvider(ctx)); v != "" {
			return v
		}
	}
	return s.baseURL
}

// SetShareService 注入 share service（用于 Complete 时写分享表）
func (s *Service) SetShareService(svc ShareServiceInterface) {
	s.shareService = svc
}

// SetPickupBinder 注入匿名取件码占位绑定（anonymous 域实现；nil = 绑定通道不可用）
func (s *Service) SetPickupBinder(b PickupBinder) { s.pickupBinder = b }

// SetStorage 注入存储服务（直传按当前激活后端落盘，含路径防御）
func (s *Service) SetStorage(st StorageWriter) {
	s.storage = st
}

// SetObjectStore 注入真预签名直传能力（s3 后端；不支持时 Init 自动回退自家中转）
func (s *Service) SetObjectStore(o ObjectStore) {
	s.objects = o
}

// InitMeta init 元信息
type InitMeta struct {
	UploadID    string    `json:"upload_id"`
	ObjectKey   string    `json:"object_key"`
	FileName    string    `json:"file_name"`
	FileSize    int64     `json:"file_size"`
	ContentType string    `json:"content_type"`
	Scheme      string    `json:"scheme"`
	ExpireAt    time.Time `json:"expire_at"`
	Complete    bool      `json:"complete"`
	// UserID/OwnerIP 会话属主（2026-11-06 起落库）。用途有二：
	//   1. Complete 的幂等回读必须按属主限定（upload_id 可被他方算出/复用）；
	//   2. 多文件绑定的 object_key 归属校验（否则可把他人对象绑成自己的分享）。
	// 历史会话这两个字段为空 ⇒ 相关判据按「无属主记录」兼容放行（窗口受
	// 会话 TTL 约束，≤1h）。
	UserID      uint   `json:"user_id"`
	OwnerIP     string `json:"owner_ip,omitempty"`
	ExpireValue int32  `json:"expire_value"`
	ExpireStyle string `json:"expire_style"`
	RequireAuth bool   `json:"require_auth"`
	// PasswordHash 为 require_auth=true 时分享密码的 bcrypt 哈希(明文不落存储)
	PasswordHash string `json:"password_hash,omitempty"`
	// FileHash 直传完成后服务端计算的 SHA-256（写入 file_codes.file_hash，秒传依据）
	// SessionTTL 直传会话/签名时效（handler 按管理配置下发；0=服务端默认 1h）
	SessionTTL time.Duration `json:"-"`
	FileHash   string        `json:"file_hash,omitempty"`
	// Transferred 数据面已完成（self 方案 PUT 落盘成功后置位）。签名时效
	// （ExpireAt）只约束 PUT **发起**时刻；数据已传完的会话 Complete 不再被
	// ExpireAt 拒绝——否则慢速大文件「传完即过期」，2.5GB 对象成孤儿
	// （2026-11-03 真机实录）。s3 方案服务端不见流量，以 Complete 的对象存在
	// 性核实为等价判据（见 Complete）。
	Transferred bool `json:"transferred,omitempty"`
	// PickupCode 匿名取件码直传绑定（2026-10-07）：非空时 Complete 不新建分享，
	// 而是把对象回填到该占位记录（/anonymous/generate 创建）。有效期/密码以
	// 占位记录为准——本 meta 的 expire/auth 字段在绑定路径被忽略。
	PickupCode string `json:"pickup_code,omitempty"`
	// Note 文件备注（可空，随分享展示给取件人；2026-10-12 #489）
	Note string `json:"note,omitempty"`
}

// InitResult init 返回
type InitResult struct {
	UploadID      string            `json:"upload_id"`
	UploadURL     string            `json:"upload_url"`
	Method        string            `json:"method"`
	Headers       map[string]string `json:"headers"`
	ExpireSeconds int32             `json:"expire_seconds"`
	ObjectKey     string            `json:"object_key"`
	Scheme        string            `json:"scheme"`
	Token         string            `json:"token"`
}

// Init 申请预签名
func (s *Service) Init(ctx context.Context, meta InitMeta) (*InitResult, error) {
	// FileSize 必须 >0（service 层兜底；CheckUploadSize 对 <=0 恒放行）
	if meta.FileSize <= 0 {
		return nil, fmt.Errorf("文件大小必须大于0")
	}
	// 类型 + 整文件大小校验（presign 为大文件直传通道，上限走
	// upload.max_file_size 而非单请求体上限；单请求体上限由 HTTP 层约束）
	if err := utils.CheckWholeFileSize(meta.FileSize); err != nil {
		// typed error（带 CodeTooLarge 业务码 + 动态文案）⇒ handler 透传 413
		// 且前端显示「文件过大: 最大允许 N 字节」，而非泛化的「预签名生成失败」
		return nil, utils.NewFileTooLargeError(utils.GetMaxFileSize())
	}
	if !utils.IsAllowedExtension(meta.FileName) {
		return nil, fmt.Errorf("该文件类型禁止上传")
	}
	// 过期样式白名单
	if err := utils.CheckExpireStyleAllowed(meta.ExpireStyle); err != nil {
		return nil, err
	}
	// 文件名消毒（展示/落库用原始名；磁盘路径与 ObjectKey 均服务端生成）
	meta.FileName = utils.SanitizeFileName(meta.FileName)

	// 1. 生成 upload_id 与服务端 ObjectKey。
	// 修复(P0)：此前 ObjectKey 恒为空串——直传写到 data 目录本身必然失败，
	// 且分享记录 FilePath 为空、整条 presign 大文件链路断裂。
	// ObjectKey 由服务端生成（客户端不可控），从构造上根除路径穿越。
	uploadID, err := genID("up")
	if err != nil {
		return nil, err
	}
	meta.UploadID = uploadID
	// 会话/签名时效：handler 按管理配置(下载设置-直传)下发，0=服务端默认 1h
	ttl := s.defaultExpire
	if meta.SessionTTL > 0 {
		ttl = meta.SessionTTL
	}
	meta.ExpireAt = s.nowTime().Add(ttl)
	meta.Complete = false
	meta.ObjectKey = genObjectKey(uploadID, meta.FileName)

	// 2. 决定直传模式（需在存 meta 前定案，meta.Scheme 随之持久化）：
	//    - 后端为 s3 且支持离线签名 → 真预签名直传（客户端 PUT 对象存储，
	//      下载/上传流量均不过服务器）
	//    - 否则回退自家中转 URL（X-Upload-Token）
	//    模式由服务端判定并无条件覆写（客户端传入的 scheme 不可信）
	token := s.signToken(uploadID, meta.ObjectKey, meta.ExpireAt)
	uploadURL := fmt.Sprintf("%s/api/v1/presign/upload-direct/%s", baseurl.Resolve(ctx, s.resolveBaseURL(ctx)), uploadID)
	headers := map[string]string{"X-Upload-Token": token}
	meta.Scheme = SchemeSelf
	if s.objects != nil {
		if u, perr := s.objects.PresignPutURL(ctx, meta.ObjectKey, ttl); perr == nil {
			uploadURL = u
			meta.Scheme = SchemeS3
			headers = map[string]string{}
			if meta.ContentType != "" {
				headers["Content-Type"] = meta.ContentType
			}
		}
	}

	// 3. 存 meta（含最终 Scheme）。
	// 会话记录寿命 ≠ 签名时效：self 方案在 PUT 发起时续期（见 UploadDirect）；
	// s3 方案的传输不经本进程、无法中途续期 ⇒ Init 就按 transferWindow 存活，
	// 否则慢速大文件传完时 meta 已被 TTL 清掉 ⇒ Complete 只能报 not found。
	metaTTL := ttl
	if meta.Scheme == SchemeS3 && metaTTL < transferWindow {
		metaTTL = transferWindow
	}
	metaJSON, _ := json.Marshal(meta)
	if err := s.rdb.Set(ctx, fmt.Sprintf(keyUploadMeta, uploadID), metaJSON, metaTTL).Err(); err != nil {
		return nil, err
	}

	return &InitResult{
		UploadID:      uploadID,
		UploadURL:     uploadURL,
		Method:        "PUT",
		Headers:       headers,
		ExpireSeconds: int32(ttl.Seconds()),
		ObjectKey:     meta.ObjectKey,
		Scheme:        meta.Scheme,
		Token:         token,
	}, nil
}

// CompleteResult Complete 返回结果（包含 share_code）
type CompleteResult struct {
	*InitMeta
	ShareCode    string
	ShareURL     string
	FullShareURL string
	OwnerIP      string
	PickupCode   string // 6 位取件码（share 域铸造；永久分享/未注入 minter 为空）
}

// Complete 完成通知（调 share service 写分享表）
func (s *Service) Complete(ctx context.Context, uploadID, token, ownerIP string) (*CompleteResult, error) {
	// 同一 upload_id 的完成面排队互斥（core#151）：下面「读 meta → 建分享 →
	// 标记完成」是 read-then-create，无锁时并发两次 Complete（超时重试/双击/
	// HTTP 重放）各自读到 Complete=false ⇒ 同一 upload_id 建出 2 条分享、
	// 回 2 个取件码 ⇒ 幂等形同虚设。
	// 拿锁**之后**才读 meta ⇒ 后到者读到的必是前一个完成后的状态 ⇒ 走下方
	// 既有的幂等回读分支 ⇒ 返回同一条分享（同属主幂等语义不变）。
	// ctx 取消/超时 ⇒ 未做任何事直接返回，客户端可重试。
	release, lockErr := uploadlock.Lock(ctx, uploadID)
	if lockErr != nil {
		return nil, lockErr
	}
	defer release()

	// 1. 读 meta
	metaJSON, err := s.rdb.Get(ctx, fmt.Sprintf(keyUploadMeta, uploadID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, ErrUploadNotFound
	}
	if err != nil {
		return nil, err
	}

	var meta InitMeta
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		return nil, err
	}

	// 2. 校验过期——**只约束数据面未完成的会话**（2026-11-03 语义修正）：
	// 签名时效管的是「PUT 必须在 ExpireAt 前发起」，不是「Complete 必须在
	// ExpireAt 前到达」。数据已传完（Transferred）的会话，用户传了多久不该
	// 决定他能不能拿到分享——否则慢速大文件传完即过期、对象成孤儿（真机
	// 2.5GB 实录）。token HMAC 在下面 3 照常校验，不因放行而松动。
	// s3 方案服务端不见流量（Transferred 无从置位），其"发起时刻"约束由预签名
	// URL 时效承担，完成面以 3.5 的对象存在性核实为真闸门 ⇒ 同样不在此拒。
	if s.nowTime().After(meta.ExpireAt) && !meta.Transferred && meta.Scheme != SchemeS3 {
		s.rdb.Del(ctx, fmt.Sprintf(keyUploadMeta, uploadID))
		return nil, ErrUploadExpired
	}

	// 3. 校验 token
	expected := s.signToken(meta.UploadID, meta.ObjectKey, meta.ExpireAt)
	if !hmac.Equal([]byte(expected), []byte(token)) {
		return nil, ErrTokenInvalid
	}

	// 3.5 s3 直传模式：向对象存储核实对象真实存在并取实际大小。
	// 服务器没有经手流量，S3 是事实源；同时兜底最大上传限制。
	// 校验失败不标记 complete，客户端可重传后再 Complete。
	if meta.Scheme == SchemeS3 {
		if s.objects == nil {
			return nil, errors.New("s3 直传模式未配置对象存储能力")
		}
		actual, _, herr := s.objects.HeadObject(ctx, meta.ObjectKey)
		if herr != nil {
			return nil, fmt.Errorf("对象尚未上传或不可读: %w", herr)
		}
		// 整文件上限走 upload.max_file_size（0=不限），与分片通道同语义——
		// 此前误用单请求体上限 GetMaxUploadSize，大文件直传在 Complete 被
		// upload_size(如 10MB) 误杀，违背该通道"突破单请求限制"的存在意义
		if err := utils.CheckWholeFileSize(actual); err != nil {
			return nil, utils.NewFileTooLargeError(utils.GetMaxFileSize())
		}
		meta.FileSize = actual
		// 注意：服务器未接触内容，meta.FileHash 保持客户端提供的值（可为空，
		// 为空则该分享不参与秒传指纹库）
	}

	// 3.6 self 方案存在性核实（v0.11.1）：对象存在性此前只有 s3 路径核实，
	// 本地中转不检查——init 后未 PUT 直接 Complete 会创建孤儿分享（记录在、
	// 对象无，公开访问 500）。此处以读能力核实对象存在并采信服务端实际大小。
	if meta.Scheme == SchemeSelf {
		if hr, ok := s.storage.(headReader); s.storage != nil && ok {
			rc, size, rerr := hr.GetFileReader(ctx, meta.ObjectKey)
			if rerr != nil {
				return nil, fmt.Errorf("对象尚未上传或不可读: %w", rerr)
			}
			_ = rc.Close()
			// 整文件上限（同 s3 路径语义），非单请求体上限
			if err := utils.CheckWholeFileSize(size); err != nil {
				return nil, utils.NewFileTooLargeError(utils.GetMaxFileSize())
			}
			meta.FileSize = size
		}
	}

	// 3.8 魔数复检（2026-10-05 审计 P3，对齐 chunk 通道）：此前 presign 通道
	// 落盘后无内容检查，扩展名伪装的可执行/脚本可经 presign 入库。统一存储
	// 实例两种 scheme 都读得到对象（local 直落盘 / s3 为事实源），失败即拒并
	// 清理已落对象。
	if hr, ok := s.storage.(headReader); s.storage != nil && ok {
		if rc, _, rerr := hr.GetFileReader(ctx, meta.ObjectKey); rerr == nil {
			buf := make([]byte, 512)
			n, _ := io.ReadFull(rc, buf)
			_ = rc.Close()
			if n > 0 {
				if cerr := utils.CheckUploadContent(meta.FileName, buf[:n]); cerr != nil {
					_ = s.storage.DeleteFile(ctx, meta.ObjectKey)
					s.rdb.Del(ctx, fmt.Sprintf(keyUploadMeta, uploadID))
					return nil, cerr
				}
			}
		}
		// 读不到内容头（远端驱动抖动）不阻断：存在性/大小已由 3.5 或上传链路核实
	}

	// 4. 校验是否已完成
	//
	// 幂等（2026-10-24 审计，core v0.32.6）：已标记完成 ⇒ **回读**该 upload_id
	// 已建的分享并返回（同一个分享码），而不是直接报错。
	//   · 修复前：二次 Complete 报错（客户端重试 ⇒ 拿不到结果）；
	//   · 修复前更糟：标记在**建分享之前**（见下）⇒ 建分享失败时 meta 已标记
	//     ⇒ 客户端重试只得 ErrAlreadyComplete ⇒ **已上传对象永久成孤儿**。
	//   · 本次把标记挪到建分享**之后** ⇒ 建分享失败 ⇒ 未标记 ⇒ 可重试；
	//     已标记 ⇒ 分享必然已存在 ⇒ 回读复用（幂等）。
	if meta.Complete {
		// 属主限定（2026-11-06 P0）：upload_id 可由 file_hash 推导 ⇒ 无限定的
		// 回读会把**他人**的分享码返回给当前调用方。meta 自带建会话时的身份
		// （登录 user_id / 匿名 owner_ip），据此判定「是不是我自己建的那条」。
		var ownerUserID *uint
		if meta.UserID != 0 {
			uid := meta.UserID
			ownerUserID = &uid
		}
		existingCode, ferr := s.shareService.FindShareByUploadIDOwnedBy(ctx, meta.UploadID, ownerUserID, ownerIP)
		if ferr == nil && existingCode != "" {
			base := baseurl.Resolve(ctx, s.resolveBaseURL(ctx))
			return &CompleteResult{
				InitMeta:     &meta,
				ShareCode:    existingCode,
				ShareURL:     baseurl.SharePageURL("", existingCode),
				FullShareURL: baseurl.SharePageURL(base, existingCode),
				OwnerIP:      ownerIP,
			}, nil
		}
		return nil, ErrAlreadyComplete
	}

	// 5. 调 share service 写分享表
	shareCode, shareURL, fullShareURL, pickupCode, shareErr := s.createShareRecord(ctx, &meta, ownerIP)
	if shareErr != nil {
		// 建分享失败 ⇒ **不标记** complete ⇒ 客户端可重试（此前先标记 ⇒ 重试
		// 只会得到 ErrAlreadyComplete ⇒ 已上传对象永久孤儿）。
		return &CompleteResult{
			InitMeta:     &meta,
			ShareCode:    "",
			ShareURL:     "",
			FullShareURL: "",
			OwnerIP:      ownerIP,
		}, fmt.Errorf("create share record: %w", shareErr)
	}

	// 6. 标记完成（**建分享之后**）：分享已落地才标记 ⇒ 失败可重试、重复调用
	// 命中上面的幂等回读分支。
	meta.Complete = true
	updatedJSON, _ := json.Marshal(meta)
	// 保留 meta 一段时间供查询（5 分钟）
	s.rdb.Set(ctx, fmt.Sprintf(keyUploadMeta, uploadID), updatedJSON, 5*time.Minute)

	return &CompleteResult{
		InitMeta:     &meta,
		ShareCode:    shareCode,
		ShareURL:     shareURL,
		FullShareURL: fullShareURL,
		OwnerIP:      ownerIP,
		PickupCode:   pickupCode,
	}, nil
}

// createShareRecord 调 share service 写分享记录
// 返回 (shareCode, shareURL, fullShareURL, error)
func (s *Service) createShareRecord(ctx context.Context, meta *InitMeta, ownerIP string) (code, shareURL, fullURL, pickupCode string, err error) {
	// 匿名取件码绑定路径（2026-10-07）：对象回填到 /anonymous/generate 的占位记录，
	// 不新建分享；有效期/密码以占位记录为准（meta 的 expire/auth 在此被忽略）。
	if meta.PickupCode != "" {
		if s.pickupBinder == nil {
			return "", "", "", "", errors.New("取件码绑定通道未启用")
		}
		shareCode, err := s.pickupBinder.ResolvePlaceholder(ctx, meta.PickupCode)
		if err != nil {
			return "", "", "", "", fmt.Errorf("取件码绑定失败: %w", err)
		}
		if err := s.pickupBinder.BindFilePath(ctx, shareCode, meta.ObjectKey, meta.FileSize, meta.Note); err != nil {
			return "", "", "", "", fmt.Errorf("占位分享回填失败: %w", err)
		}
		// 分享页链接必须是 **hash 路由形态**（pkg/baseurl/shareurl.go）：
		// 此前此处把 API 路径 "/share/select/?code=XXX" 当页面链接返回，前端
		// 拿它做兜底转换 ⇒ origin/#/share/select/?code=XXX ⇒ vue-router 把
		// "select" 当成分享码 ⇒ 打开就是「分享不存在」；且该形态让
		// SplitSharePageURL 剥不出 base ⇒ presign complete 的 download_url
		// 拼成 "/share/select/?code=XXX/share/download?code=…" 垃圾串
		// （2026-10-23 跨仓审计）。这里给真正的分享页链接（8 位分享码）。
		base := baseurl.Resolve(ctx, s.resolveBaseURL(ctx))
		return shareCode,
			baseurl.SharePageURL("", shareCode),
			baseurl.SharePageURL(base, shareCode),
			meta.PickupCode, nil
	}
	if s.shareService == nil {
		// share service 未注入：返回 mock 数据（用于单测 / 未配置场景）
		// 形态同样走 hash 路由（真相源 pkg/baseurl/shareurl.go）。
		mockCode := "mock_" + meta.UploadID
		return mockCode,
			baseurl.SharePageURL("", mockCode),
			baseurl.SharePageURL(baseurl.Resolve(ctx, s.resolveBaseURL(ctx)), mockCode),
			"", nil
	}

	// 计算过期时间（与 share.ShareTextWithAuth 行为一致）
	expireTime := utils.CalculateExpireTime(int(meta.ExpireValue), meta.ExpireStyle)
	expireCount := utils.CalculateExpireCount(meta.ExpireStyle, int(meta.ExpireValue))

	uploadType := "presign_anonymous"
	var userIDPtr *uint
	if meta.UserID != 0 {
		uid := meta.UserID
		userIDPtr = &uid
		uploadType = "presign_authenticated"
	}

	if meta.RequireAuth && meta.PasswordHash == "" {
		// 防御:历史 init 记录可能无密码哈希,拒绝创建"密码保护形同虚设"的分享
		return "", "", "", "", errors.New("该上传未设置访问密码，无法完成分享")
	}

	req := &ShareCreateReq{
		Channel:      "presign",
		FilePath:     meta.ObjectKey,
		Size:         meta.FileSize,
		Text:         utils.SanitizeFileName(meta.FileName),
		ExpiredAt:    expireTime,
		ExpiredCount: expireCount,
		RequireAuth:  meta.RequireAuth,
		PasswordHash: meta.PasswordHash,
		UserID:       userIDPtr,
		UploadType:   uploadType,
		OwnerIP:      ownerIP,
		FileHash:     meta.FileHash,
		UploadID:     meta.UploadID,
		Note:         meta.Note,
	}

	resp, err := s.shareService.CreateShare(ctx, req)
	if err != nil {
		return "", "", "", "", err
	}

	return resp.Code, resp.ShareURL, resp.FullShareURL, resp.PickupCode, nil
}

// Abort 取消
func (s *Service) Abort(ctx context.Context, uploadID, token string) error {
	metaJSON, err := s.rdb.Get(ctx, fmt.Sprintf(keyUploadMeta, uploadID)).Result()
	if errors.Is(err, redis.Nil) {
		return ErrUploadNotFound
	}
	if err != nil {
		return err
	}
	var meta InitMeta
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		return err
	}
	expected := s.signToken(meta.UploadID, meta.ObjectKey, meta.ExpireAt)
	if !hmac.Equal([]byte(expected), []byte(token)) {
		return ErrTokenInvalid
	}
	return s.rdb.Del(ctx, fmt.Sprintf(keyUploadMeta, uploadID)).Err()
}

// GetMeta 查询（管理后台用）
func (s *Service) GetMeta(ctx context.Context, uploadID string) (*InitMeta, error) {
	metaJSON, err := s.rdb.Get(ctx, fmt.Sprintf(keyUploadMeta, uploadID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, ErrUploadNotFound
	}
	if err != nil {
		return nil, err
	}
	var meta InitMeta
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// UploadDirect 处理预签名直传：校验 token → 流式写文件（meta.ObjectKey 路径）。
// 由 bootstrap 注册的 PUT /api/v1/presign/upload-direct/:uploadID 端点调用。
// body 流式消费（配合 hertz StreamRequestBody），大文件不再整体进内存；
// 写入字节数必须与 init 申报的 meta.FileSize 完全一致，超出即拒并清理。
func (s *Service) UploadDirect(ctx context.Context, uploadID, token string, body io.Reader, declaredSize int64) error {
	// 1. 读 meta
	meta, err := s.GetMeta(ctx, uploadID)
	if err != nil {
		return err
	}
	// 2. 校验过期
	if s.nowTime().After(meta.ExpireAt) {
		s.rdb.Del(ctx, fmt.Sprintf(keyUploadMeta, uploadID))
		return ErrUploadExpired
	}
	// 3. 校验 token
	expected := s.signToken(meta.UploadID, meta.ObjectKey, meta.ExpireAt)
	if !hmac.Equal([]byte(expected), []byte(token)) {
		return ErrTokenInvalid
	}
	// 3.5 已完成会话拒绝重放覆盖（2026-10-08 攻击面加固 P0）：Complete 之后
	// meta 仍存活 5 分钟、令牌在 ExpireAt 前始终有效，持旧令牌重放 PUT 可静默
	// 替换已发布分享背后的对象——分享码不变、内容掉包。与 Complete 的
	// ErrAlreadyComplete 同语义（handler 映射 409）。
	if meta.Complete {
		return ErrAlreadyComplete
	}
	if meta.FileSize <= 0 {
		return fmt.Errorf("文件大小必须大于0")
	}
	// 3.6 内容预检（2026-11-03）：类型/魔数拒绝必须发生在**写入前**——此前只在
	// Complete 复检，用户传 1.35GB 传完才报「文件内容与扩展名不符」，带宽全白费
	// （真机实录：.AppImage 误杀场景）。Peek 不消费流（缓冲字节原样下行）；
	// 不足 512 字节按实际头部判。Complete 复检保留为兜底（S3 直传不经本函数）。
	br := bufio.NewReader(body)
	if head, _ := br.Peek(512); len(head) > 0 {
		if cerr := utils.CheckUploadContent(meta.FileName, head); cerr != nil {
			return cerr
		}
	}
	body = br
	// 4. 流式写入 + 内容哈希：超出申报大小立即截断报错（防多传绕过大小校验）
	hasher := sha256.New()
	limited := io.LimitReader(body, meta.FileSize+1)
	tee := io.TeeReader(limited, hasher)

	var written int64
	if ss, ok := s.storage.(streamWriter); ok {
		if written, err = ss.SaveStream(ctx, meta.ObjectKey, tee, meta.FileSize); err != nil {
			return fmt.Errorf("write file failed: %w", err)
		}
	} else if s.storage != nil {
		// 其他 StorageInterface 实现（无流式能力）：退化为缓冲写
		data, rerr := io.ReadAll(tee)
		if rerr != nil {
			return fmt.Errorf("read body failed: %w", rerr)
		}
		written = int64(len(data))
		if err := s.storage.SaveBytes(ctx, meta.ObjectKey, data); err != nil {
			return fmt.Errorf("write file failed: %w", err)
		}
	} else {
		// 未注入存储：本地盘直写（纯单测/降级形态）
		dataPath := "./data"
		targetPath := filepath.Join(dataPath, meta.ObjectKey)
		if !filepath.IsLocal(filepath.Clean(targetPath)) ||
			filepath.Clean(targetPath) == filepath.Clean(dataPath) ||
			!strings.HasPrefix(filepath.Clean(targetPath), filepath.Clean(dataPath)+string(filepath.Separator)) {
			return fmt.Errorf("illegal object key")
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return fmt.Errorf("create dir failed: %w", err)
		}
		f, ferr := os.Create(targetPath)
		if ferr != nil {
			return fmt.Errorf("write file failed: %w", ferr)
		}
		written, err = io.Copy(f, tee)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("write file failed: %w", err)
		}
	}
	if written != meta.FileSize {
		// 大小不符：清掉半成品
		if s.storage != nil {
			_ = s.storage.DeleteFile(ctx, meta.ObjectKey)
		} else {
			_ = os.Remove(filepath.Join("./data", meta.ObjectKey))
		}
		return fmt.Errorf("文件大小与申报不符（实际 %d / 申报 %d）", written, meta.FileSize)
	}
	// 5. 内容 SHA-256 回写 meta（Complete 时写入 file_codes.file_hash）+
	//    置位 Transferred（数据面完成标记，见 InitMeta.Transferred）。
	//    ⚠️ 这次回写必须**无条件**落盘且带 completeGrace：此前按
	//    `remaining = time.Until(ExpireAt)` 且仅在 remaining>0 时回写——慢速
	//    上传传完时 ExpireAt 早已过去 ⇒ **回写被整个跳过** ⇒ Transferred/
	//    FileHash 全丢、会话无从证明"数据已传完"（2026-11-03 真机 2.5GB 实录）。
	meta.FileHash = hex.EncodeToString(hasher.Sum(nil))
	meta.Transferred = true
	if updated, merr := json.Marshal(meta); merr == nil {
		s.rdb.Set(ctx, fmt.Sprintf(keyUploadMeta, uploadID), updated, completeGrace)
	}
	return nil
}

// genObjectKey 服务端生成存储相对路径：uploads/YYYY/MM/DD/<uploadID><消毒后扩展名>。
func genObjectKey(uploadID, fileName string) string {
	now := time.Now()
	ext := strings.ToLower(filepath.Ext(fileName))
	return filepath.Join("uploads", now.Format("2006"), now.Format("01"), now.Format("02"),
		uploadID+ext)
}

// QuickUploadResult 秒传命中结果
type QuickUploadResult struct {
	ShareCode    string
	FullShareURL string
}

// QuickShareMeta 秒传建分享所需元数据（presign init 请求携带）。
type QuickShareMeta struct {
	FileName     string
	FileSize     int64
	FileHash     string
	ExpireValue  int
	ExpireStyle  string
	RequireAuth  bool
	PasswordHash string
	Note         string // 文件备注（可空，取件页展示；2026-10-12 #489）
}

// CheckQuickUpload 秒传检查（2026-10-12 重做，D1=A 同用户内去重）：
// 命中**调用者自己**历史同 hash+size 文件时，新建一条分享（调用者元数据：
// 名称/过期/密码）引用既有存储对象，免直传直接出码。
//
// 此前语义=回借既有分享的 code（全库匹配）——两处不可接受：跨用户匹配是
// 存在性 oracle（撞库探测谁传过什么）；借来的分享生命周期/密码/过期全归
// 原主，本次请求的元数据被静默丢弃。现改为同用户 + 建新分享。
func (s *Service) CheckQuickUpload(ctx context.Context, userID uint, meta QuickShareMeta) (*QuickUploadResult, error) {
	if userID == 0 || meta.FileHash == "" || meta.FileSize <= 0 || s.shareService == nil {
		return nil, errors.New("no quick upload candidate")
	}
	fc, err := s.fileCodeRepo.FindQuickUploadSource(ctx, userID, meta.FileHash, meta.FileSize)
	if err != nil || fc == nil {
		return nil, errors.New("not found")
	}
	srcPath := fc.GetFilePath()
	if srcPath == "" {
		return nil, errors.New("not found")
	}
	// 新分享引用源对象（FilePath=源路径；share 域 CreateShare 去重钩子对同路径
	// no-op）。快照语义：新分享过期/密码独立于源分享，源删除不影响本分享
	// （物理对象由引用计数保护，删除前查引用）。
	resp, err := s.shareService.CreateShare(ctx, &ShareCreateReq{
		Channel:      "presign",
		FilePath:     srcPath,
		Size:         meta.FileSize,
		Text:         meta.FileName,
		ExpiredAt:    utils.CalculateExpireTime(meta.ExpireValue, meta.ExpireStyle),
		ExpiredCount: utils.CalculateExpireCount(meta.ExpireStyle, meta.ExpireValue),
		RequireAuth:  meta.RequireAuth,
		PasswordHash: meta.PasswordHash,
		UserID:       &userID,
		UploadType:   "authenticated",
		FileHash:     meta.FileHash,
		Note:         meta.Note,
	})
	if err != nil {
		return nil, err
	}
	return &QuickUploadResult{
		ShareCode:    resp.Code,
		FullShareURL: resp.FullShareURL,
	}, nil
}

// ============ 内部 ============

// signToken 生成 HMAC token
//
//	token = hex(hmac-sha256(signingKey, uploadID|objectKey|expireAt.Unix))
func (s *Service) signToken(uploadID, objectKey string, expireAt time.Time) string {
	mac := hmac.New(sha256.New, s.signingKey)
	_, _ = fmt.Fprintf(mac, "%s|%s|%d", uploadID, objectKey, expireAt.Unix())
	return hex.EncodeToString(mac.Sum(nil))
}

// genID presign 会话/分片 ID（wire 格式 "<prefix>_"+24 hex 逐字不变）。
//
// presignIDEntropyBytes 会话 ID 的熵预算：12 字节 = 96 bit 熵 ⇒ 2^96 防碰撞
// 预算（core#86 收编 kitx/uidgen 时定长于此；会话 ID 兼作幂等键，见
// 取件计次语义第 15 条——重试按 upload_id 回读复用，碰撞即误并会话）。
func genID(prefix string) (string, error) {
	const presignIDEntropyBytes = 12
	id, err := uidgen.RandomHexE(presignIDEntropyBytes)
	if err != nil {
		return "", err
	}
	return prefix + "_" + id, nil
}
