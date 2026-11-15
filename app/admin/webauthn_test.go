package admin

// core#160 管理员 Passkey：挑战生成、attestation/assertion 校验正负例、
// 登录闸挂接语义。驱动全部走 webauthn_simulator_test.go 的软认证器。

import (
	"context"
	"encoding/base64"
	"strconv"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/totp"
	"github.com/pigeonbox/core/repo/db/model"
)

// testWebAuthnRP 固定 RP 参数（模拟器按同一组参数造应答）。
var testWebAuthnRP = WebAuthnRP{
	RPID:        "share.example.com",
	Origins:     []string{"https://share.example.com"},
	DisplayName: "PigeonBoxTest",
}

const testWebAuthnOrigin = "https://share.example.com"

func newWebAuthnTestService(t *testing.T) *Service {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.User{}, &model.WebAuthnCredential{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	// :memory: 每条连接独立库 ⇒ 钉死单连接
	sqlDB.SetMaxOpenConns(1)
	return NewService(g)
}

// registerPasskey 走完整登记两步，返回软认证器（后续登录负例复用同一把钥匙）。
func registerPasskey(t *testing.T, svc *Service, u *model.User) *virtualAuthenticator {
	t.Helper()
	ctx := context.Background()
	creation, err := svc.WebAuthnRegisterOptions(ctx, u.ID, testWebAuthnRP)
	require.NoError(t, err)
	sim := newVirtualAuthenticator(t)
	body := sim.createResponse(t, creation.Response.Challenge.String(), testWebAuthnRP.RPID, testWebAuthnOrigin)
	row, err := svc.WebAuthnRegisterVerify(ctx, u.ID, testWebAuthnRP, creation.Response.Challenge.String(), body, "测试钥匙")
	require.NoError(t, err)
	require.NotNil(t, row)
	return sim
}

// ===== 登记流 =====

func TestWebAuthnRegisterFlow(t *testing.T) {
	svc := newWebAuthnTestService(t)
	ctx := context.Background()
	u, _ := seedTotpAdmin(t, svc, "wa_reg_admin", false)

	t.Run("①挑战生成并可完成登记", func(t *testing.T) {
		creation, err := svc.WebAuthnRegisterOptions(ctx, u.ID, testWebAuthnRP)
		require.NoError(t, err)
		require.NotNil(t, creation)
		challenge := creation.Response.Challenge.String()
		require.NotEmpty(t, challenge, "challenge 必须下发")
		assert.GreaterOrEqual(t, len(challenge), 16, "challenge 至少 16 字节 base64url")

		sim := newVirtualAuthenticator(t)
		body := sim.createResponse(t, challenge, testWebAuthnRP.RPID, testWebAuthnOrigin)
		row, err := svc.WebAuthnRegisterVerify(ctx, u.ID, testWebAuthnRP, challenge, body, "办公钥匙")
		require.NoError(t, err, "模拟器 attestation 必须通过库校验")
		assert.Equal(t, u.ID, row.UserID)
		assert.Equal(t, base64.RawURLEncoding.EncodeToString(sim.credID), row.CredentialID)
		assert.Equal(t, "办公钥匙", row.Name)
		assert.NotEmpty(t, row.PublicKey, "公钥必须落库")

		rows, err := svc.webauthnRepo.ListByUser(ctx, u.ID)
		require.NoError(t, err)
		require.Len(t, rows, 1)
	})

	t.Run("②challenge 一次性：登记成功后同 challenge 再用即拒", func(t *testing.T) {
		creation, err := svc.WebAuthnRegisterOptions(ctx, u.ID, testWebAuthnRP)
		require.NoError(t, err)
		challenge := creation.Response.Challenge.String()
		sim := newVirtualAuthenticator(t)
		body := sim.createResponse(t, challenge, testWebAuthnRP.RPID, testWebAuthnOrigin)
		_, err = svc.WebAuthnRegisterVerify(ctx, u.ID, testWebAuthnRP, challenge, body, "k")
		require.NoError(t, err)
		_, err = svc.WebAuthnRegisterVerify(ctx, u.ID, testWebAuthnRP, challenge, body, "k")
		require.ErrorIs(t, err, ErrWebAuthnInvalid, "同一 challenge 不得消费两次")
	})

	t.Run("③负例：attestation 被篡改必须拒", func(t *testing.T) {
		creation, err := svc.WebAuthnRegisterOptions(ctx, u.ID, testWebAuthnRP)
		require.NoError(t, err)
		challenge := creation.Response.Challenge.String()
		sim := newVirtualAuthenticator(t)
		body := mutateCredential(t, sim.createResponse(t, challenge, testWebAuthnRP.RPID, testWebAuthnOrigin), tamperAttestationObject)
		_, err = svc.WebAuthnRegisterVerify(ctx, u.ID, testWebAuthnRP, challenge, body, "k")
		require.ErrorIs(t, err, ErrWebAuthnInvalid)
	})

	t.Run("④负例：origin 不在白名单必须拒", func(t *testing.T) {
		creation, err := svc.WebAuthnRegisterOptions(ctx, u.ID, testWebAuthnRP)
		require.NoError(t, err)
		challenge := creation.Response.Challenge.String()
		sim := newVirtualAuthenticator(t)
		body := sim.createResponse(t, challenge, testWebAuthnRP.RPID, "https://evil.example")
		_, err = svc.WebAuthnRegisterVerify(ctx, u.ID, testWebAuthnRP, challenge, body, "k")
		require.ErrorIs(t, err, ErrWebAuthnInvalid)
	})

	t.Run("⑤负例：他人 challenge 不能替我登记（串号）", func(t *testing.T) {
		other, _ := seedTotpAdmin(t, svc, "wa_reg_other", false)
		creation, err := svc.WebAuthnRegisterOptions(ctx, other.ID, testWebAuthnRP)
		require.NoError(t, err)
		challenge := creation.Response.Challenge.String()
		sim := newVirtualAuthenticator(t)
		body := sim.createResponse(t, challenge, testWebAuthnRP.RPID, testWebAuthnOrigin)
		_, err = svc.WebAuthnRegisterVerify(ctx, u.ID, testWebAuthnRP, challenge, body, "k")
		require.ErrorIs(t, err, ErrWebAuthnInvalid, "userID 与 challenge 发起人不一致必须拒")
	})

	t.Run("⑥负例：同一张 passkey 重复登记必须拒", func(t *testing.T) {
		sim := registerPasskey(t, svc, seedSecondAdmin(t, svc, "wa_reg_dup"))
		creation, err := svc.WebAuthnRegisterOptions(ctx, u.ID, testWebAuthnRP)
		require.NoError(t, err)
		challenge := creation.Response.Challenge.String()
		body := sim.createResponse(t, challenge, testWebAuthnRP.RPID, testWebAuthnOrigin)
		_, err = svc.WebAuthnRegisterVerify(ctx, u.ID, testWebAuthnRP, challenge, body, "再登一次")
		require.ErrorIs(t, err, ErrWebAuthnInvalid, "唯一索引前的查重必须拦下重复凭证")
	})
}

// seedSecondAdmin 便于多子测试互不撞唯一索引的管理员。
func seedSecondAdmin(t *testing.T, svc *Service, username string) *model.User {
	t.Helper()
	u, _ := seedTotpAdmin(t, svc, username, false)
	return u
}

// ===== 登录流 =====

func TestWebAuthnLoginFlow(t *testing.T) {
	svc := newWebAuthnTestService(t)
	ctx := context.Background()
	u, _ := seedTotpAdmin(t, svc, "wa_login_admin", false)
	sim := registerPasskey(t, svc, u)
	userHandle := []byte(strconv.FormatUint(uint64(u.ID), 10))

	t.Run("①options 下发真实 allowCredentials", func(t *testing.T) {
		assertion, err := svc.WebAuthnLoginOptions(ctx, "wa_login_admin", testWebAuthnRP)
		require.NoError(t, err)
		require.NotNil(t, assertion)
		require.Len(t, assertion.Response.AllowedCredentials, 1)
		assert.Equal(t, sim.credID, []byte(assertion.Response.AllowedCredentials[0].CredentialID))
		assert.NotEmpty(t, assertion.Response.Challenge.String())
	})

	t.Run("②assertion 校验通过并签发管理员 JWT", func(t *testing.T) {
		assertion, err := svc.WebAuthnLoginOptions(ctx, "wa_login_admin", testWebAuthnRP)
		require.NoError(t, err)
		challenge := assertion.Response.Challenge.String()
		body := sim.getResponse(t, challenge, testWebAuthnRP.RPID, testWebAuthnOrigin, userHandle)
		token, err := svc.WebAuthnLoginVerify(ctx, testWebAuthnRP, challenge, body)
		require.NoError(t, err, "模拟器断言必须通过库校验")
		require.NotEmpty(t, token)
		claims, err := auth.ParseToken(token)
		require.NoError(t, err)
		assert.Equal(t, "admin", claims.Role, "必须签发管理员角色")
		assert.Equal(t, u.ID, claims.UserID)

		// 计数器与最近使用时间回写（克隆判据的持久面）
		rows, err := svc.webauthnRepo.ListByUser(ctx, u.ID)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Positive(t, rows[0].SignCount, "签名计数必须回写")
		assert.NotNil(t, rows[0].LastUsedAt)
	})

	t.Run("③challenge 一次性：成功后再用即拒", func(t *testing.T) {
		assertion, err := svc.WebAuthnLoginOptions(ctx, "wa_login_admin", testWebAuthnRP)
		require.NoError(t, err)
		challenge := assertion.Response.Challenge.String()
		body := sim.getResponse(t, challenge, testWebAuthnRP.RPID, testWebAuthnOrigin, userHandle)
		_, err = svc.WebAuthnLoginVerify(ctx, testWebAuthnRP, challenge, body)
		require.NoError(t, err)
		_, err = svc.WebAuthnLoginVerify(ctx, testWebAuthnRP, challenge, body)
		require.ErrorIs(t, err, ErrWebAuthnInvalid)
	})

	t.Run("④负例：签名被篡改必须拒", func(t *testing.T) {
		assertion, err := svc.WebAuthnLoginOptions(ctx, "wa_login_admin", testWebAuthnRP)
		require.NoError(t, err)
		challenge := assertion.Response.Challenge.String()
		body := mutateCredential(t, sim.getResponse(t, challenge, testWebAuthnRP.RPID, testWebAuthnOrigin, userHandle), tamperSignature)
		_, err = svc.WebAuthnLoginVerify(ctx, testWebAuthnRP, challenge, body)
		require.ErrorIs(t, err, ErrWebAuthnInvalid)
	})

	t.Run("⑤负例：origin 伪造必须拒", func(t *testing.T) {
		assertion, err := svc.WebAuthnLoginOptions(ctx, "wa_login_admin", testWebAuthnRP)
		require.NoError(t, err)
		challenge := assertion.Response.Challenge.String()
		body := sim.getResponse(t, challenge, testWebAuthnRP.RPID, "https://evil.example", userHandle)
		_, err = svc.WebAuthnLoginVerify(ctx, testWebAuthnRP, challenge, body)
		require.ErrorIs(t, err, ErrWebAuthnInvalid)
	})

	t.Run("⑥防枚举：未知用户名发同形诱饵且验证必拒", func(t *testing.T) {
		assertion, err := svc.WebAuthnLoginOptions(ctx, "no_such_admin", testWebAuthnRP)
		require.NoError(t, err, "诱饵挑战必须与真挑战同形返回（不暴露用户名存在性）")
		require.NotNil(t, assertion)
		require.Len(t, assertion.Response.AllowedCredentials, 1, "allowCredentials 形状与真挑战一致")
		challenge := assertion.Response.Challenge.String()
		// 用真实钥匙对诱饵挑战签名也过不了（userID=0 诱饵在 Finish 前即拒）
		body := sim.getResponse(t, challenge, testWebAuthnRP.RPID, testWebAuthnOrigin, userHandle)
		_, err = svc.WebAuthnLoginVerify(ctx, testWebAuthnRP, challenge, body)
		require.ErrorIs(t, err, ErrWebAuthnInvalid)
	})

	t.Run("⑦负例：登记 challenge 不能当登录 challenge 消费", func(t *testing.T) {
		creation, err := svc.WebAuthnRegisterOptions(ctx, u.ID, testWebAuthnRP)
		require.NoError(t, err)
		challenge := creation.Response.Challenge.String()
		body := sim.getResponse(t, challenge, testWebAuthnRP.RPID, testWebAuthnOrigin, userHandle)
		_, err = svc.WebAuthnLoginVerify(ctx, testWebAuthnRP, challenge, body)
		require.ErrorIs(t, err, ErrWebAuthnInvalid, "kind 校验必须拦下跨流消费")
	})
	// 注意：⑦ 消费掉的登记 challenge 不影响后续（各子测试自带新 challenge）
}

// ===== 登录闸挂接（GenerateTokenForAdmin） =====

func TestAdminLoginWebAuthnGate(t *testing.T) {
	svc := newWebAuthnTestService(t)
	ctx := context.Background()

	prev := conf.Snapshot()
	t.Cleanup(func() { conf.SetGlobalConfig(prev) })
	setGate := func(on bool) {
		conf.SetGlobalConfig(&conf.AppConfiguration{
			Security: conf.SecurityConfig{RequireAdminWebAuthn: on},
		})
	}
	enroll := func(u *model.User) {
		require.NoError(t, svc.webauthnRepo.Create(ctx, &model.WebAuthnCredential{
			UserID: u.ID, CredentialID: base64.RawURLEncoding.EncodeToString([]byte(u.Username + "-cred")),
			PublicKey: []byte{0x58, 0x20, 0x01}, Name: "k",
		}))
	}

	t.Run("①默认关：已登记 passkey 的纯口令登录不受影响", func(t *testing.T) {
		setGate(false)
		u, pw := seedTotpAdmin(t, svc, "wa_gate_off", false)
		enroll(u)
		token, err := svc.GenerateTokenForAdmin(ctx, u.Username, pw, "")
		require.NoError(t, err, "默认关必须与存量登录语义一致")
		assert.NotEmpty(t, token)
	})

	t.Run("②开闸：已登记 + 纯口令 → ErrWebAuthnRequired（40012 流程信号）", func(t *testing.T) {
		setGate(true)
		u, pw := seedTotpAdmin(t, svc, "wa_gate_on", false)
		enroll(u)
		_, err := svc.GenerateTokenForAdmin(ctx, u.Username, pw, "")
		require.ErrorIs(t, err, ErrWebAuthnRequired)
	})

	t.Run("③开闸：未登记的管理员不受影响（开闸不锁死存量）", func(t *testing.T) {
		setGate(true)
		u, pw := seedTotpAdmin(t, svc, "wa_gate_nocred", false)
		token, err := svc.GenerateTokenForAdmin(ctx, u.Username, pw, "")
		require.NoError(t, err)
		assert.NotEmpty(t, token)
	})

	t.Run("④开闸：口令+二验（TOTP/恢复码兜底）仍放行", func(t *testing.T) {
		setGate(true)
		u, pw := seedTotpAdmin(t, svc, "wa_gate_totp", true)
		enroll(u)
		code, err := totp.Code(u.TotpSecret, time.Now())
		require.NoError(t, err)
		token, err := svc.GenerateTokenForAdmin(ctx, u.Username, pw, code)
		require.NoError(t, err, "第二因子兜底必须放行（issue 恢复码口径）")
		assert.NotEmpty(t, token)
		// 缺码仍是既有 40012 语义（口令对了只缺码）
		_, err = svc.GenerateTokenForAdmin(ctx, u.Username, pw, "")
		require.ErrorIs(t, err, ErrTotpRequired)
	})

	t.Run("⑤开闸：口令错误仍走凭据错误（先于一切闸）", func(t *testing.T) {
		setGate(true)
		u, _ := seedTotpAdmin(t, svc, "wa_gate_badpw", false)
		enroll(u)
		_, err := svc.GenerateTokenForAdmin(ctx, u.Username, "wrong-password", "")
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrWebAuthnRequired, "口令错不许泄露 Passkey 闸状态")
	})
}

// ===== RP 推导 =====

func TestResolveWebAuthnRP(t *testing.T) {
	t.Run("①配置钉死优先", func(t *testing.T) {
		rp := ResolveWebAuthnRP(conf.WebAuthnConfig{
			RPID: "pinned.example.com", RPOrigins: []string{"https://pinned.example.com"},
		}, "share.example.com:56790", "http", "", "")
		assert.Equal(t, "pinned.example.com", rp.RPID)
		assert.Equal(t, []string{"https://pinned.example.com"}, rp.Origins)
	})

	t.Run("②缺省按请求推导：Host 去端口 + XFP/Origin 并集", func(t *testing.T) {
		rp := ResolveWebAuthnRP(conf.WebAuthnConfig{}, "share.example.com:56790", "http", "https", "https://share.example.com")
		assert.Equal(t, "share.example.com", rp.RPID, "RP ID 不含端口")
		assert.Contains(t, rp.Origins, "https://share.example.com:56790", "推导 origin 用 XFP 的 scheme")
		assert.Contains(t, rp.Origins, "https://share.example.com", "浏览器 Origin 头必须并入")
		assert.Equal(t, "PigeonBox", rp.DisplayName)
	})

	t.Run("③非法 XFP 归一为 http（不外溢）", func(t *testing.T) {
		rp := ResolveWebAuthnRP(conf.WebAuthnConfig{}, "a.example.com", "http", "gopher", "")
		assert.Contains(t, rp.Origins, "http://a.example.com")
	})

	t.Run("④空 Host 不产空 origin", func(t *testing.T) {
		rp := ResolveWebAuthnRP(conf.WebAuthnConfig{}, "", "http", "", "")
		assert.Empty(t, rp.RPID)
		assert.Empty(t, rp.Origins)
	})
}

// ===== challenge 存取原语 =====

func TestWebAuthnCeremonyStore(t *testing.T) {
	s := &webAuthnCeremonyStore{m: make(map[string]*webAuthnCeremony)}
	newCer := func(kind webAuthnCeremonyKind) *webAuthnCeremony {
		return &webAuthnCeremony{kind: kind, userID: 1, expires: time.Now().Add(time.Minute)}
	}

	s.put("ch1", newCer(webAuthnCeremonyRegister))
	got, ok := s.take("ch1", webAuthnCeremonyRegister)
	require.True(t, ok)
	require.NotNil(t, got)
	_, ok = s.take("ch1", webAuthnCeremonyRegister)
	assert.False(t, ok, "一次性：消费后即删")

	s.put("ch2", newCer(webAuthnCeremonyRegister))
	_, ok = s.take("ch2", webAuthnCeremonyLogin)
	assert.False(t, ok, "kind 不符必须拒（且已消费）")

	s.put("ch3", &webAuthnCeremony{kind: webAuthnCeremonyLogin, expires: time.Now().Add(-time.Second)})
	_, ok = s.take("ch3", webAuthnCeremonyLogin)
	assert.False(t, ok, "过期必须拒")
}
