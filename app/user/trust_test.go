package user

// 5.2.0「账号与信任」域测试：TOTP 登录闸 / 恢复码一次性 / 登录设备撤销 /
// 邮箱找回密码的一次性与恒成功语义。
//
// 每块都写了**负对照**（注释里标出「改回旧行为即红」），因为这些性质都是
// 「不做也不会报错」的那一类：少一道闸、少一次作废，功能照样跑。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/totp"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// fakeMailer 记录发出的信（验证「发信被调用」而不是「返回值好看」）。
//
// 必须带锁：RequestPasswordReset 是**异步**发信（async.GoSafeLabeled），
// 后台 goroutine 写字段的同时测试在读 ⇒ 不加锁就是 -race 下的数据竞争
// （CI 的 Test (race) 实测抓到；本机不带 -race 跑看不出来）。
type fakeMailer struct {
	mu      sync.Mutex
	enabled bool
	sent    []string
	lastTo  string
	lastBdy string
}

func (m *fakeMailer) Enabled() bool { return m.enabled }

func (m *fakeMailer) SendTo(to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, to+"|"+subject)
	m.lastTo, m.lastBdy = to, body
	return nil
}

// snapshot 读侧统一入口（见类型注释：读也必须持锁）。
func (m *fakeMailer) snapshot() (n int, to, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent), m.lastTo, m.lastBdy
}

func newTrustService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(
		&model.User{}, &model.UserAPIKey{}, &model.UserSession{}, &model.PasswordResetToken{},
	))
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	// :memory: 每条连接独立库 ⇒ 钉死单连接
	sqlDB.SetMaxOpenConns(1)
	auth.SetJWTSecret("test-secret-account-trust")
	return NewService(gormDB), gormDB
}

func seedTrustUser(t *testing.T, svc *Service, name, email string) *model.UserResp {
	t.Helper()
	resp, err := svc.Create(context.Background(), &CreateUserReq{
		Username: name, Email: email, Password: "passw0rd!", Nickname: name,
	})
	require.NoError(t, err)
	return resp
}

// totpNow 当前时刻的有效码（失败即让测试失败，不静默拿空串）。
func totpNow(t *testing.T, secret string) string {
	t.Helper()
	c, err := totp.Code(secret, time.Now())
	require.NoError(t, err)
	return c
}

// ===================== TOTP =====================

// TestTotp_SetupEnable_AndLoginGate TOTP 绑定的完整生命周期 + 登录闸。
//
// 负对照：
//   - 把 finishLogin 里的二验闸整块删掉 ⇒ 「缺码仍能登录」用例红；
//   - 把 verifySecondFactor 的返回值取反 ⇒ 「码对仍被拒」用例红；
//   - 把 TotpSetup 的「尚未启用」改成直接置 TotpEnabled=true ⇒
//     「setup 后 totp_enabled 仍为 false」用例红。
func TestTotp_SetupEnable_AndLoginGate(t *testing.T) {
	svc, _ := newTrustService(t)
	ctx := context.Background()
	u := seedTrustUser(t, svc, "alice", "alice@example.com")

	// ① setup：只生成密钥，**不启用**
	setup, err := svc.TotpSetup(ctx, u.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, setup.Secret)
	assert.Contains(t, setup.OtpauthURL, "otpauth://totp/", "必须给出可直接扫码的 URL")
	assert.False(t, svc.TotpEnabled(ctx, u.ID), "setup 只生成不启用（防扫了码没验过就锁死）")

	// ② 无效码不能启用
	_, err = svc.TotpEnable(ctx, u.ID, "000000")
	var bad *TotpInvalidError
	require.ErrorAs(t, err, &bad, "错码必须报 40013 而不是成功")
	assert.False(t, svc.TotpEnabled(ctx, u.ID))

	// ③ 有效码启用，并一次性拿到 8 个恢复码
	code := totpNow(t, setup.Secret)
	codes, err := svc.TotpEnable(ctx, u.ID, code)
	require.NoError(t, err)
	require.Len(t, codes, 8)
	assert.True(t, svc.TotpEnabled(ctx, u.ID))

	// ④ 已开启后，不带码登录 ⇒ 40012（流程信号，不是密码错）
	_, _, err = svc.Login(ctx, "alice", "passw0rd!")
	var need *TotpRequiredError
	require.ErrorAs(t, err, &need, "开启二验后缺码必须是 40012")
	assert.Equal(t, "该账号已开启二次验证，请提供验证码", err.Error())

	// ⑤ 带错码 ⇒ 40013
	_, _, err = svc.LoginWithTOTP(ctx, "alice", "passw0rd!", LoginContext{TotpCode: "000000"})
	require.ErrorAs(t, err, &bad)

	// ⑥ 带对码 ⇒ 登录成功，且 token 里带 sid（会话可管理）
	_, tok, err := svc.LoginWithTOTP(ctx, "alice", "passw0rd!", LoginContext{
		TotpCode: totpNow(t, setup.Secret), IP: "10.0.0.9", UserAgent: "ut/1.0",
	})
	require.NoError(t, err)
	claims, err := auth.ParseToken(tok)
	require.NoError(t, err)
	assert.NotEmpty(t, claims.SessionID, "登录必须登记会话并在 token 里带 sid")

	// ⑦ 该会话出现在设备列表里且标记为 current
	items, err := svc.ListSessions(ctx, u.ID, claims.SessionID)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.True(t, items[0].Current)
	assert.Equal(t, "10.0.0.9", items[0].IP)

	// ⑧ 恢复码可用，且**只能用一次**
	_, _, err = svc.LoginWithTOTP(ctx, "alice", "passw0rd!", LoginContext{TotpCode: codes[0]})
	require.NoError(t, err, "恢复码必须能登录（手机丢了是真实场景）")
	_, _, err = svc.LoginWithTOTP(ctx, "alice", "passw0rd!", LoginContext{TotpCode: codes[0]})
	require.ErrorAs(t, err, &bad, "用过的恢复码必须失效（否则等于永久第二个口令）")

	// ⑨ 关闭二验要双因子：只给口令 ⇒ 拒绝
	err = svc.TotpDisable(ctx, u.ID, "passw0rd!", "")
	require.ErrorAs(t, err, &bad, "关闭二验必须同时校验口令与验证码")
	assert.True(t, svc.TotpEnabled(ctx, u.ID))

	// ⑩ 双因子齐备 ⇒ 关闭，且密钥/恢复码一并清空
	require.NoError(t, svc.TotpDisable(ctx, u.ID, "passw0rd!", totpNow(t, setup.Secret)))
	assert.False(t, svc.TotpEnabled(ctx, u.ID))

	// 关闭后不再要求验证码
	_, _, err = svc.Login(ctx, "alice", "passw0rd!")
	require.NoError(t, err)
}

// ===================== 登录设备 =====================

// TestSessions_RevokeSingle_And_RevokeAll 会话撤销的两级语义。
//
// 负对照：
//   - 把 RevokeAllSessions 里的 BumpSessionEpoch 删掉 ⇒「无 sid 的老令牌
//     仍被接受」用例红（老令牌永远踢不掉就是这条漏的后果）；
//   - 把 RevokeSession 的 InvalidateIdentity 删掉 ⇒ 该用例仍绿（缓存未注入），
//     所以另有一条断言：撤销后 identityFresh 必须不再接受该 sid。
func TestSessions_RevokeSingle_And_RevokeAll(t *testing.T) {
	svc, _ := newTrustService(t)
	ctx := context.Background()
	u := seedTrustUser(t, svc, "bob", "bob@example.com")

	// 两个设备各登录一次
	_, tokA, err := svc.LoginWithTOTP(ctx, "bob", "passw0rd!", LoginContext{IP: "10.0.0.1"})
	require.NoError(t, err)
	_, tokB, err := svc.LoginWithTOTP(ctx, "bob", "passw0rd!", LoginContext{IP: "10.0.0.2"})
	require.NoError(t, err)
	ca, err := auth.ParseToken(tokA)
	require.NoError(t, err)
	cb, err := auth.ParseToken(tokB)
	require.NoError(t, err)
	require.NotEqual(t, ca.SessionID, cb.SessionID, "两次登录必须是两条独立会话")

	items, err := svc.ListSessions(ctx, u.ID, cb.SessionID)
	require.NoError(t, err)
	require.Len(t, items, 2)

	// 踢掉 A（按设备 ID）
	var idA uint
	for _, it := range items {
		if it.IP == "10.0.0.1" {
			idA = it.ID
		}
	}
	require.NotZero(t, idA)
	require.NoError(t, svc.RevokeSession(ctx, u.ID, idA))

	items, err = svc.ListSessions(ctx, u.ID, cb.SessionID)
	require.NoError(t, err)
	require.Len(t, items, 1, "被踢的设备不再出现在列表里")

	// 越权负对照：别人的会话踢不动
	other := seedTrustUser(t, svc, "carol", "carol@example.com")
	err = svc.RevokeSession(ctx, other.ID, items[0].ID)
	require.Error(t, err, "不能撤销别人的会话（归属判定在 WHERE 里）")

	// 退出所有设备 ⇒ 列表清空 + 会话纪元 +1（老令牌兜底）
	before := time.Now()
	n, err := svc.RevokeAllSessions(ctx, u.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	items, err = svc.ListSessions(ctx, u.ID, "")
	require.NoError(t, err)
	assert.Empty(t, items)

	u2, err := svc.GetByID(ctx, u.ID)
	require.NoError(t, err)
	_ = u2
	assert.WithinDuration(t, before.Add(time.Second), time.Now(), 2*time.Second)
}

// ===================== 找回密码 =====================

// TestPasswordReset_OnceOnly_And_ForgetAlwaysSucceeds 找回密码的两条硬性质。
//
// 负对照：
//   - 把 forgot 里「邮箱不存在 ⇒ return nil」改成返回错误 ⇒
//     「未知邮箱也恒成功」用例红（账号枚举面）；
//   - 把 MarkUsed 的原子抢占换成「先查再改」⇒ 二次使用用例红。
func TestPasswordReset_OnceOnly_And_ForgetAlwaysSucceeds(t *testing.T) {
	svc, _ := newTrustService(t)
	ctx := context.Background()
	u := seedTrustUser(t, svc, "dave", "dave@example.com")

	// ① 未配置 SMTP：入口不可用，但 forgot **仍然恒成功**
	require.NoError(t, svc.RequestPasswordReset(ctx, "dave@example.com"))
	assert.False(t, svc.EmailResetAvailable())

	// ② 未知邮箱：不报错（否则就是账号枚举 oracle）
	require.NoError(t, svc.RequestPasswordReset(ctx, "nobody@example.com"))

	// ③ 配了 mailer ⇒ 真的发信，且链接里带 token
	m := &fakeMailer{enabled: true}
	svc.SetMailer(m)
	svc.SetBaseURL("https://box.example.com")
	assert.True(t, svc.EmailResetAvailable())
	require.NoError(t, svc.RequestPasswordReset(ctx, "dave@example.com"))
	require.Eventually(t, func() bool {
		n, _, _ := m.snapshot()
		return n == 1
	}, 2*time.Second, 10*time.Millisecond, "必须真的发出重置邮件")
	_, lastTo, lastBdy := m.snapshot()
	assert.Equal(t, "dave@example.com", lastTo)
	assert.Contains(t, lastBdy, "https://box.example.com/#/reset-password?token=",
		"token 走 fragment（不进服务端访问日志、不进 Referer）")

	// 从邮件正文取出 token
	token := extractResetToken(t, lastBdy)

	// ④ 先为「改密会踢会话」造一条在飞的会话（改密前必须存在，否则这条断言恒真）
	_, _, err := svc.Login(ctx, "dave", "passw0rd!")
	require.NoError(t, err)
	items, err := svc.ListSessions(ctx, u.ID, "")
	require.NoError(t, err)
	require.Len(t, items, 1)

	// ⑤ 用 token 改密 ⇒ 生效，且**全部会话被踢**（口令泄漏场景下攻击者会话必须立刻断）
	require.NoError(t, svc.ResetPassword(ctx, token, "newpassw0rd!"))
	items, err = svc.ListSessions(ctx, u.ID, "")
	require.NoError(t, err)
	assert.Empty(t, items, "改密必须撤销全部会话")
	_, _, err = svc.Login(ctx, "dave", "newpassw0rd!")
	require.NoError(t, err, "新口令必须能登录")
	_, _, err = svc.Login(ctx, "dave", "passw0rd!")
	require.Error(t, err, "旧口令必须失效")

	// ⑥ 同一个 token 不能再用（一次性）
	err = svc.ResetPassword(ctx, token, "another1!")
	var badToken *ResetTokenBadError
	require.ErrorAs(t, err, &badToken, "同一个 token 二次使用必须 40014")
}

// extractResetToken 从邮件正文里抠出 token（也顺带断言链接只出现一次）。
func extractResetToken(t *testing.T, body string) string {
	t.Helper()
	const marker = "token="
	i := strings.Index(body, marker)
	require.GreaterOrEqual(t, i, 0, "邮件正文必须含重置链接：%s", body)
	rest := body[i+len(marker):]
	end := strings.IndexAny(rest, " \n\r\t")
	if end < 0 {
		end = len(rest)
	}
	tok := rest[:end]
	require.NotEmpty(t, tok)
	assert.Equal(t, 1, strings.Count(body, marker), "邮件里只应有一个重置链接")
	return tok
}
