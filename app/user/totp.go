package user

// 二次验证（TOTP，RFC 6238）——5.2.0「账号与信任」。
//
// # 三条设计红线
//
//  1. **setup 只生成不启用**：必须再送一次有效码（enable）才算绑定成功。
//     否则「扫了码但客户端算错了时间」的用户会把自己永久锁在门外。
//  2. **disable 双因子**（口令 + 当前码）：令牌被盗后顺手关掉二验是最常见的
//     绕过路径；只验口令等于二验白开。
//  3. **恢复码只在 enable 这一刻返回一次**，库内只存 bcrypt 哈希。
//     手机丢失是真实场景，但没有恢复码的二验 = 单点故障。
//
// # 为什么明文存 TotpSecret
//
// 加密需要一把比 JWT secret 更稳定的密钥，而 JWT secret 允许运维更换
// ⇒ 换一次即全员二验静默失效（更糟：没有人会注意）。能读 users 表者本已能读
// 全部对象存储凭据，明文不构成新的信任边界。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/internal/authn"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/totp"
	"github.com/pigeonbox/core/repo/db/model"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/oekit/kitx/uidgen"
)

// TotpRequiredError 账号已开启二验但本次登录未提供验证码。
//
// 这是**流程信号**（40012），不是失败：前端据此弹第二个输入框，
// 不应计入「登录失败」文案，也不应触发锁定提示。
type TotpRequiredError struct{}

func (e *TotpRequiredError) Error() string {
	return "该账号已开启二次验证，请提供验证码"
}
func (e *TotpRequiredError) ErrCode() int { return errcode.CodeTotpRequired }

// TotpInvalidError 验证码不正确 / 已使用过（40013）。
type TotpInvalidError struct{ Msg string }

func (e *TotpInvalidError) Error() string {
	if strings.TrimSpace(e.Msg) != "" {
		return e.Msg
	}
	return "验证码不正确"
}
func (e *TotpInvalidError) ErrCode() int { return errcode.CodeTotpInvalid }

// LoginContext 登录的附加信息（二验码 + 会话设备信息）。
//
// 会话信息（IP/UA）在这里而不是在 handler 里单独传：它们与「这次登录登记哪一行
// 会话」是同一件事，分开传必然出现「token 有 sid 但会话行没写 IP」的半成品。
type LoginContext struct {
	TotpCode  string
	IP        string
	UserAgent string
}

// TotpSetupResult 绑定第一步的返回值。
type TotpSetupResult struct {
	Secret     string
	OtpauthURL string
}

// TotpSetup 第一步：生成密钥并回 otpauth:// URL（**尚未启用**）。
//
// 每次调用都重新生成并覆盖：过期未完成的绑定不该留下可用的旧密钥。
func (s *Service) TotpSetup(ctx context.Context, userID uint) (*TotpSetupResult, error) {
	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return nil, errParam("用户不存在")
	}
	if u.TotpEnabled {
		return nil, errParam("二次验证已开启，请先关闭再重新绑定")
	}
	secret, err := totp.GenerateSecret()
	if err != nil {
		return nil, err
	}
	u.TotpSecret = secret
	u.TotpRecoveryCodes = ""
	if err := s.repo.Update(ctx, u); err != nil {
		return nil, err
	}
	return &TotpSetupResult{
		Secret:     secret,
		OtpauthURL: totp.ProvisioningURL("PigeonBox", u.Username, secret),
	}, nil
}

// TotpEnable 第二步：校验一次有效码后启用，并**一次性**返回恢复码。
func (s *Service) TotpEnable(ctx context.Context, userID uint, code string) ([]string, error) {
	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return nil, errParam("用户不存在")
	}
	if u.TotpEnabled {
		return nil, errParam("二次验证已开启")
	}
	if strings.TrimSpace(u.TotpSecret) == "" {
		return nil, errParam("请先获取绑定密钥")
	}
	if !totp.Validate(u.TotpSecret, code, time.Now()) {
		return nil, &TotpInvalidError{}
	}

	codes, hashes, err := generateRecoveryHashes()
	if err != nil {
		return nil, err
	}
	u.TotpEnabled = true
	u.TotpRecoveryCodes = hashes
	if err := s.repo.Update(ctx, u); err != nil {
		return nil, err
	}
	return codes, nil
}

// TotpDisable 关闭二验（口令 + 当前码双因子）。
func (s *Service) TotpDisable(ctx context.Context, userID uint, password, code string) error {
	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return errParam("用户不存在")
	}
	if !u.TotpEnabled {
		return errParam("二次验证未开启")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return &TotpInvalidError{Msg: "密码错误"}
	}
	// 关闭时接受 TOTP 码或任一未用过的恢复码：手机丢了还得能关掉二验。
	if !totp.Validate(u.TotpSecret, code, time.Now()) {
		rest, ok := authn.ConsumeRecoveryCode(u.TotpRecoveryCodes, code)
		if !ok {
			return &TotpInvalidError{}
		}
		u.TotpRecoveryCodes = rest
	}
	u.TotpEnabled = false
	u.TotpSecret = ""
	u.TotpRecoveryCodes = ""
	return s.repo.Update(ctx, u)
}

// verifySecondFactor 校验二验码（TOTP 优先，其次恢复码）——薄委托。
//
// 判据本体在 internal/authn（2026-11-14 评审 S01 抽出，admin 登录同用）；
// 保留本方法仅为既有调用点（finishLogin）与测试零改动。
func (s *Service) verifySecondFactor(ctx context.Context, userID uint, code string) (bool, error) {
	return authn.VerifySecondFactor(ctx, s.repo, userID, code)
}

// TotpEnabled 该用户是否已开启二次验证（/user/info 显隐安全设置入口用）。
//
// 查不到用户时返回 false 而不是错误：这是**展示位**，用户信息主流程不该
// 因为一个可选安全设置读失败而整体 500。
func (s *Service) TotpEnabled(ctx context.Context, userID uint) bool {
	u, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return false
	}
	return u.TotpEnabled
}

// generateRecoveryHashes 生成 8 个恢复码并回传明文 + 待落库的哈希 JSON。
func generateRecoveryHashes() ([]string, string, error) {
	codes, err := totp.GenerateRecoveryCodes(8)
	if err != nil {
		return nil, "", err
	}
	hashes := make([]string, 0, len(codes))
	for _, c := range codes {
		// 落库的是**归一化后**的形态（去分隔符、全大写）：校验侧同样先归一化
		// 再比对 ⇒ 用户抄成 "abcdefgh" 或漏了连字符照样能进。哈希原始形态会让
		// 「抄错格式」被当成「码不对」，而它其实是同一个码。
		h, err := bcrypt.GenerateFromPassword([]byte(totp.NormalizeRecoveryCode(c)), bcrypt.DefaultCost)
		if err != nil {
			return nil, "", err
		}
		hashes = append(hashes, string(h))
	}
	raw, err := json.Marshal(hashes)
	if err != nil {
		return nil, "", err
	}
	return codes, string(raw), nil
}

// LoginWithTOTP 口令 + （可选的）二次验证码登录，并登记一条登录会话。
//
// 会话登记失败**不阻断登录**：会话表是「可管理性」能力，不是认证前提；
// 为它挡住登录会让一次建表/迁移问题演变成全员锁在门外。
// 降级后 token 无 sid，认证侧只走纪元判定（与升级前的老令牌同款语义）。
func (s *Service) LoginWithTOTP(ctx context.Context, username, password string, lc LoginContext) (*model.UserResp, string, error) {
	u, err := s.repo.GetByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", ErrBadCredentials
		}
		return nil, "", err
	}
	return s.finishLogin(ctx, u, password, lc)
}

// finishLogin 口令校验 → 二验闸 → 登记会话并签发令牌。
//
// 抽成一处是为了让「用户名登录」与「邮箱登录」共享**同一条**闸门：
// 两条独立实现必然漂移，而这里漂移的后果是某个入口绕过二验。
func (s *Service) finishLogin(ctx context.Context, u *model.User, password string, lc LoginContext) (*model.UserResp, string, error) {
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return nil, "", ErrBadCredentials
	}
	if u.Status != "active" {
		return nil, "", ErrAccountDisabled
	}

	// 二验闸：口令正确才走到这里 —— 二验码缺失/错误都算「已通过口令的一半」，
	// 因此调用方需要按不同 errcode 分流（40012 继续问码 / 40013 计入失败）。
	if u.TotpEnabled {
		code := strings.TrimSpace(lc.TotpCode)
		if code == "" {
			return nil, "", &TotpRequiredError{}
		}
		ok, err := s.verifySecondFactor(ctx, u.ID, code)
		if err != nil {
			return nil, "", err
		}
		if !ok {
			return nil, "", &TotpInvalidError{}
		}
	}

	return s.issueSession(ctx, u, lc)
}

// issueSession 登记会话行 + 签发带 sid 的令牌。
func (s *Service) issueSession(ctx context.Context, u *model.User, lc LoginContext) (*model.UserResp, string, error) {
	sid := ""
	if s.sessionRepo != nil {
		sid = uidgen.RandomHex(16)
		row := &model.UserSession{
			UserID:     u.ID,
			SessionID:  sid,
			IP:         truncate(lc.IP, 45),
			UserAgent:  truncate(lc.UserAgent, 255),
			LastSeenAt: time.Now(),
		}
		if err := s.sessionRepo.Create(ctx, row); err != nil {
			logger.CtxWarn(ctx, "create login session failed, degrade to sid-less token",
				"user_id", u.ID, "err", err)
			sid = ""
		}
	}

	tok, err := auth.GenerateTokenWithSession(u.ID, u.Username, u.Role, sid)
	if err != nil {
		return nil, "", errors.New("生成令牌失败")
	}
	// 新会话必须立刻对认证侧可见：身份缓存里可能还留着「该用户零会话」的快照
	// （30s TTL）⇒ 不清就直接用自己的新 token 401。
	middleware.InvalidateIdentity(u.ID)

	return u.ToResp(), tok, nil
}

// truncate 按字符数截断（列宽限定），避免超长 UA 撑爆写入。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
