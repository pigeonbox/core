package user

// 邮箱自助找回密码（5.2.0）。
//
// # 两条不可退让的性质
//
//  1. **forgot 恒成功**：无论邮箱是否存在、SMTP 是否配置、发信是否失败，
//     对外一律 200。任何差异化的响应都会把「这个邮箱注册过本站」这份
//     信息卖给任意访客（账号枚举），而对真实用户毫无帮助。
//     唯一例外（core#119）：邮箱**格式**非法按参数错误 400——纯输入校验
//     （与 BindAndValidate 同类），不泄露注册状态。
//  2. **令牌一次性 + 短窗口**：sha256 落库、30 分钟过期、MarkUsed 原子
//     抢占（同 token 并发提交两次只有一次成功）。邮件里的链接是最容易被
//     转发/截图的东西，把它变成可重复使用的凭据等于开一条后门。
//
// # 为什么不是「发新密码到邮箱」
//
// 明文口令走邮件 = 口令永久留在邮件服务器与转发链里。发一次性链接则要求
// 持有链接的人自己设新口令，泄漏面收敛到 30 分钟。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pigeonbox/contracts/errcode"
	pkgerrors "github.com/pigeonbox/core/pkg/errors"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/model"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/kitx/uidgen"
	"github.com/oekit/pkg/encoding"
)

// resetTokenTTL 找回链接有效期。
//
// 30 分钟：够用户从邮件点进来，不够让一封躺了三天的转发邮件变成可用凭据。
const resetTokenTTL = 30 * time.Minute

// resetPasswordMinLen 新口令最小长度（找回路径的兜底；与注册/改密同口径）。
const resetPasswordMinLen = 6

// ResetTokenBadError 找回令牌无效/过期/已使用（40014）。
//
// 三种失败合成一个码是**故意**的：区分它们等于告诉持有者「这个 token 曾存在」，
// 而用户能采取的行动完全相同——重新申请一次。
type ResetTokenBadError struct{}

func (e *ResetTokenBadError) Error() string { return "链接已失效，请重新申请找回密码" }
func (e *ResetTokenBadError) ErrCode() int  { return errcode.CodeResetTokenBad }

// RequestPasswordReset 申请找回密码（**恒成功**，见文件头；格式非法除外）。
func (s *Service) RequestPasswordReset(ctx context.Context, email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil
	}
	// core#119：格式非法是**参数错误**（400），不属于「恒成功」——恒成功只
	// 覆盖账号枚举 oracle（邮箱是否存在 / SMTP 是否可用 / 发信是否失败），
	// 格式非法不泄露任何注册状态，且注入类输入不该静默走查库流程。
	if err := utils.ValidateEmail(email); err != nil {
		return pkgerrors.BadRequest(err.Error())
	}
	u, err := s.repo.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 邮箱没注册：什么也不做，但也不能让调用方看出来（恒成功）
			return nil
		}
		// 查询本身出错同样不能外露（否则「快 vs 慢」也是 oracle）；
		// 记日志给运维，对外仍成功。
		logger.CtxWarn(ctx, "password reset lookup failed", "err", err)
		return nil
	}
	if u.Status != "active" {
		return nil
	}
	if s.resetRepo == nil || s.mailer == nil || !s.mailer.Enabled() {
		// 未配置 SMTP：前端按 capabilities.email_password_reset 早就隐藏了入口，
		// 走到这里只可能是直接打 API ⇒ 仍然恒成功，只记一条日志。
		logger.CtxWarn(ctx, "password reset requested but mail channel disabled", "user_id", u.ID)
		return nil
	}

	plain := uidgen.RandomHex(32)
	row := &model.PasswordResetToken{
		UserID:    u.ID,
		TokenHash: encoding.Sha256(plain),
		ExpiresAt: time.Now().Add(resetTokenTTL),
	}
	if err := s.resetRepo.Create(ctx, row); err != nil {
		logger.CtxWarn(ctx, "persist password reset token failed", "user_id", u.ID, "err", err)
		return nil
	}

	link := s.resetLink(plain)
	body := fmt.Sprintf("您好 %s：\n\n有人（希望是您）在 PigeonBox 申请重置密码。点击下面的链接设置新密码：\n\n%s\n\n"+
		"链接 %d 分钟内有效，且只能使用一次。若这不是您发起的，请忽略本邮件\n——您的密码与账号不会有任何变化。\n\n—— PigeonBox",
		u.Username, link, int(resetTokenTTL.Minutes()))

	// 发信走异步（禁裸 go，panic 会被收编上报）：SMTP 慢或挂掉不能把
	// 「我已提交申请」这个响应拖到超时。
	async.GoSafeLabeled("password-reset-mail", func() {
		if err := s.mailer.SendTo(u.Email, "PigeonBox 密码重置", body); err != nil {
			logger.Error("send password reset mail failed", zap.Uint("user_id", u.ID), zap.Error(err))
		}
	})
	return nil
}

// resetLink 拼找回链接（前端路由）。
//
// 用 hash 路由 + fragment 携带 token：fragment **不会**发到服务端访问日志，
// 也不会出现在 Referer 里（比 query 更安全）。
func (s *Service) resetLink(token string) string {
	base := strings.TrimRight(strings.TrimSpace(s.baseURL), "/")
	return fmt.Sprintf("%s/#/reset-password?token=%s", base, token)
}

// ResetPassword 用一次性令牌设置新口令。
//
// 成功后必须做三件事，缺一即留后门：
//
//	① 该用户**其余**未用令牌全部作废（旧链接不能继续可用）；
//	② 会话纪元 +1 + 全部会话撤销（口令泄漏场景下，攻击者的会话必须立刻断）；
//	③ 清身份缓存（否则撤销要等 30s 才生效）。
func (s *Service) ResetPassword(ctx context.Context, token, newPassword string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return &ResetTokenBadError{}
	}
	if len(newPassword) < resetPasswordMinLen {
		return fmt.Errorf("新密码至少 %d 位", resetPasswordMinLen)
	}
	if s.resetRepo == nil {
		return &ResetTokenBadError{}
	}

	row, err := s.resetRepo.GetByHash(ctx, encoding.Sha256(token))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &ResetTokenBadError{}
		}
		return err
	}
	if row.UsedAt != nil || time.Now().After(row.ExpiresAt) {
		return &ResetTokenBadError{}
	}
	// 原子抢占：并发两次提交同一个 token，只有一次能拿到 RowsAffected=1
	ok, err := s.resetRepo.MarkUsed(ctx, row.ID, time.Now())
	if err != nil {
		return err
	}
	if !ok {
		return &ResetTokenBadError{}
	}

	u, err := s.repo.GetByID(ctx, row.UserID)
	if err != nil {
		return &ResetTokenBadError{}
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	u.PasswordHash = string(hashed)
	if err := s.repo.Update(ctx, u); err != nil {
		return err
	}

	_ = s.resetRepo.InvalidateAllForUser(ctx, u.ID, time.Now())
	if s.sessionRepo != nil {
		_, _ = s.sessionRepo.RevokeAll(ctx, u.ID, 0, time.Now())
	}
	if err := s.repo.BumpSessionEpoch(ctx, u.ID); err == nil {
		middleware.InvalidateIdentity(u.ID)
	}
	return nil
}
