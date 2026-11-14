// Package authn 认证共享构件：admin/user 两域共用的第二因子判据。
//
// 为什么单独成包（2026-11-14 评审 S01）：TOTP 判据此前只活在 app/user，
// admin 登录若复制一份实现必然漂移——而漂移的后果是某个登录入口绕过 MFA
// （S01 漏洞本体：POST /admin/login 完全不查 TotpEnabled）。判据只许有一份。
package authn

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/totp"
	"github.com/pigeonbox/core/repo/db/model"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// UserStore 第二因子所需的最小用户存取（admin/user 两域共用，窄接口）。
// 两边的 *dao.UserRepository 都满足：GetByID 读二验状态/密钥，Update 核销恢复码。
type UserStore interface {
	GetByID(ctx context.Context, id uint) (*model.User, error)
	Update(ctx context.Context, u *model.User) error
}

// VerifySecondFactor 校验 TOTP 或单次恢复码（恢复码命中即核销）。
//
// 恢复码命中即作废（一次性）：把用过的恢复码留着等于把「第二个口令」永久留在
// 一份可能已泄漏的纸上。
//
// 从 app/user 迁出（2026-11-14 评审 S01）：admin 登录必须走同一判据，
// 否则开 MFA 的管理员绕 /admin/login 即绕 MFA。
func VerifySecondFactor(ctx context.Context, store UserStore, userID uint, code string) (bool, error) {
	u, err := store.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	if totp.Validate(u.TotpSecret, code, time.Now()) {
		return true, nil
	}
	rest, ok := ConsumeRecoveryCode(u.TotpRecoveryCodes, code)
	if !ok {
		return false, nil
	}
	u.TotpRecoveryCodes = rest
	if err := store.Update(ctx, u); err != nil {
		return false, err
	}
	logger.CtxWarn(ctx, "totp recovery code consumed", "user_id", userID)
	return true, nil
}

// ConsumeRecoveryCode 在哈希清单里找匹配项；返回剩余清单与是否命中。
//
// 归一化后再比：用户抄下来的是 ABCD-EFGH，输入时可能带空格、小写、
// 或者把连字符丢了 —— 这些都不该算「码不对」。
//
// 从 app/user 迁出（同 S01 抽取）：除登录二验外，TotpDisable 的
// 「口令 + 码」关闭路径也消费恢复码，同一份实现两处共用。
func ConsumeRecoveryCode(stored, input string) (string, bool) {
	if strings.TrimSpace(stored) == "" {
		return stored, false
	}
	var hashes []string
	if err := json.Unmarshal([]byte(stored), &hashes); err != nil {
		return stored, false
	}
	norm := totp.NormalizeRecoveryCode(input)
	if norm == "" {
		return stored, false
	}
	for i, h := range hashes {
		if bcrypt.CompareHashAndPassword([]byte(h), []byte(norm)) == nil {
			rest := append(append([]string{}, hashes[:i]...), hashes[i+1:]...)
			raw, err := json.Marshal(rest)
			if err != nil {
				return stored, false
			}
			return string(raw), true
		}
	}
	return stored, false
}
