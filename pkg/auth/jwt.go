package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/oekit/pkg/secretx"
)

var (
	ErrInvalidToken = errors.New("invalid token")
	ErrExpiredToken = errors.New("token has expired")
	ErrTokenRevoked = errors.New("token has been revoked")

	// jwtSecret 持有 HS256 签名密钥（secretx.SecureString：XOR 混淆存储，随机
	// 密钥逐字节异或，Set/Clear 双向零化旧值，自带 RWMutex ⇒ 本包不再需要额外
	// 的密钥锁）。
	//
	// 为什么不留裸 []byte：签名密钥是进程生命周期内常驻的 crypto 材料，裸切片在
	// heap dump / 内存扫描里是**连续明文**（strings 直接可读）；混淆后拿到的是密文。
	// 为什么是 SecureString 而不是 SecureBytes：SecureBytes 只做副本隔离、**存储
	// 仍是明文**（上游 doc 明说「副本隔离存取」），拿它顶替等于什么都没换——
	// 本目录的用例 TestJWTSecret_NotStoredInPlaintext 会当场转红。
	//
	// 定位如实声明：这是**内存扫描缓解，不是加密**——能读进程内存的攻击者只多
	// 一步还原。静态落库另走存储加密，两套原语不互相冒充。
	jwtSecret = secretx.NewSecureString("PigeonBox2025SecretKey")
)

// getSecret 取当前密钥的**独立副本**（并发安全由 SecureBytes 内部锁保证）。
//
// 为什么必须返回副本：旧实现把内部切片本身交出去，调用方（jwt 库或将来任何
// 调用点）就地改写/清零改的就是签名密钥本体 ⇒ 全站已签发令牌无痕失效，且
// 没有任何日志线索。副本隔离把这个隐患从「沉默」变成「不可能」。
// 明文只在调用方手里存活一次签名/验签的时长（Get 内部已解出，这里再复制一份
// 给 jwt 库当 hmac key），用完即被 GC 回收。
func getSecret() []byte { return []byte(jwtSecret.Get()) }

// Claims JWT claims
type Claims struct {
	UserID   uint   `json:"user_id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	// Epoch 会话纪元快照：签发时用户的 session_epoch。改密/封禁/降权会使
	// DB 纪元 +1，纪元不匹配的旧 token 即时失效（认证中间件回查比对）。
	Epoch int `json:"sess_epoch,omitempty"`
	// SessionID 登录会话标识（5.2.0）：对应 user_sessions 一行。
	// 空 = 升级前签发的老令牌（或会话表故障时的降级签发）⇒ 认证侧跳过
	// 会话回查，只走纪元判定 —— 否则升级瞬间全员被踢下线。
	SessionID string `json:"sid,omitempty"`
	jwt.RegisteredClaims
}

// epochLoader 签发时读取用户当前会话纪元（composition root 注入 dao 桥；
// 无注入时恒 0——纯单测环境语义不变）。
var (
	epochLoaderMu sync.RWMutex
	epochLoader   func(userID uint) int
)

// SetEpochLoader 注入纪元加载器（bootstrap 调用一次；nil 关闭）。
func SetEpochLoader(fn func(userID uint) int) {
	epochLoaderMu.Lock()
	defer epochLoaderMu.Unlock()
	epochLoader = fn
}

func currentEpoch(userID uint) int {
	epochLoaderMu.RLock()
	fn := epochLoader
	epochLoaderMu.RUnlock()
	if fn == nil {
		return 0
	}
	return fn(userID)
}

// defaultSessionExpiry 会话时长默认值：7 天。
const defaultSessionExpiry = 7 * 24 * time.Hour

// sessionExpiryNanos 当前会话时长（纳秒原子存储），默认 defaultSessionExpiry。
// 管理后台"用户配置→会话过期时间"通过 SetSessionExpiry 在线调整
// （此前硬编码 168h，配置项形同虚设）。
//
// core#153：热更写（SetSessionExpiry）与签发读（GenerateTokenWithSession）此前
// 作用于**裸包级 time.Duration 变量** ⇒ -race 实锤 DATA RACE（无同步边，最坏
// 签发出混合口径 ExpiresAt）。改 atomic.Int64 存纳秒：无锁 ⇒ 不存在锁序/死锁面，
// 写 1 处 + 读 2 处全部经 Store/Load 收口（对照同文件 epochLoader 的 RWMutex 模式，
// 单值热更用原子量更轻）。
var sessionExpiryNanos atomic.Int64

func init() { sessionExpiryNanos.Store(int64(defaultSessionExpiry)) }

// SetSessionExpiry 设置会话时长；<=0 视为非法，保留原值。
// 仅影响新签发的 token，已签发会话到自然过期。
func SetSessionExpiry(d time.Duration) {
	if d <= 0 {
		return
	}
	sessionExpiryNanos.Store(int64(d))
}

// SessionExpiry 当前会话时长（测试/诊断用）。
func SessionExpiry() time.Duration { return time.Duration(sessionExpiryNanos.Load()) }

// GenerateToken 生成 JWT token（不带会话标识：兼容既有调用点）。
func GenerateToken(userID uint, username, role string) (string, error) {
	return GenerateTokenWithSession(userID, username, role, "")
}

// GenerateTokenWithSession 生成带会话标识的 JWT（登录/刷新令牌时使用）。
// sessionID 为空时退化为无 sid 令牌（会话表写入失败也照常登录）。
func GenerateTokenWithSession(userID uint, username, role, sessionID string) (string, error) {
	claims := &Claims{
		UserID:    userID,
		Username:  username,
		Role:      role,
		SessionID: sessionID,
		Epoch:     currentEpoch(userID),
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(SessionExpiry())),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "PigeonBox",
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(getSecret())
}

// ParseToken 解析 JWT token
func ParseToken(tokenString string) (*Claims, error) {
	secret := getSecret()
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		return secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrExpiredToken
		}
		return nil, ErrInvalidToken
	}
	if claims, ok := token.Claims.(*Claims); ok && token.Valid {
		return claims, nil
	}
	return nil, ErrInvalidToken
}

// RefreshToken 刷新 token。已注销（黑名单）的 token 拒绝刷新，
// 堵住"登出后旧 token 仍可换新"的撤销绕过（设计文档 §9.6）。
//
// 轮换语义（2026-10-05 审计 P1 修复）：签发新 token 的同时把旧 token 按剩余
// 有效期加入黑名单——否则被盗 token 可与真用户并行使用到自然过期，且每次
// 刷新都无法止损。
//
// sid 继承（core#106）：新 token **继承旧 token 的会话标识**。此前这里调
// GenerateToken（GenerateTokenWithSession(..., "")）⇒ 刷新一次就把 sid 洗掉
// ⇒ 认证侧的会话回查（identityFresh 对空 sid 整段跳过）对新令牌永久失效，
// 「踢设备」止损被刷新绕过。注意本函数只负责「同 sid 重签」；纪元/会话存活
// 复核在调用方（RefreshSession 按 identityFresh 同款口径先行把关）。
func RefreshToken(ctx context.Context, tokenString string) (string, error) {
	claims, err := ParseToken(tokenString)
	if err != nil {
		return "", err
	}
	if IsTokenRevoked(ctx, tokenString) {
		return "", ErrTokenRevoked
	}
	newToken, err := GenerateTokenWithSession(claims.UserID, claims.Username, claims.Role, claims.SessionID)
	if err != nil {
		return "", err
	}
	if remaining := time.Until(claims.ExpiresAt.Time); remaining > 0 {
		RevokeToken(ctx, tokenString, remaining)
	}
	return newToken, nil
}

// GenerateAdminToken 生成管理员 token
func GenerateAdminToken(userID uint, username string) (string, error) {
	return GenerateToken(userID, username, "admin")
}

// SetJWTSecret 设置 JWT secret（从配置注入，热更路径也走这里）。
// Set 内部先零化旧值与旧 XOR 密钥再重存；空串等价 Clear（与既有语义一致：
// 空密钥签发的令牌验签必然失败）。
func SetJWTSecret(secret string) { jwtSecret.Set(secret) }
