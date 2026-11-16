package admin

// 管理员 Passkey（WebAuthn，core#160）：登记 / 登录两条流的后端实现。
//
// # 形态（issue #160 拍板：最小可交付 = 核心后端流，前端 UI 留后续）
//
//	登记（需管理员 JWT）：POST /admin/webauthn/register/options → /verify
//	登录（公开面）      ：POST /admin/webauthn/login/options → /verify
//	                    （签发与 /admin/login 同形的管理员 JWT + 会话 Cookie）
//
// # 与既有登录的关系（默认不影响存量）
//
//	security.require_admin_webauthn 默认 false ⇒ 口令/TOTP 路径语义一字不变。
//	开闸后仅约束「已登记 passkey 的管理员」：纯口令不再放行（ErrWebAuthnRequired，
//	handler 映射 40012 流程信号、不计失败），须走 passkey 或提供第二因子
//	（TOTP/恢复码兜底——issue「已有恢复码可复用为兜底」口径）。
//	未登记 passkey 的管理员不受影响 ⇒ 开闸不会锁死存量账号；
//	极端兜底（passkey 丢失且未开 TOTP）= 运维关闸重进。
//
// # 依赖
//
//	github.com/go-webauthn/webauthn v0.18.2（BSD-3-Clause）：CBOR/COSE/
//	attestation/assertion 的编解码与校验全部由库承担，本仓不手写协议格式。
//
// # challenge 存取
//
//	进程内一次性 map（5 分钟 TTL，命中即删）。挑战是短命秘密，不落库；
//	代价是 admin 面多副本部署时各副本不共享 challenge ⇒ 需会话粘滞
//	（或后续落库——拆分项，PR 注明）。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// 登记/登录链路的类型化错误。
var (
	// ErrWebAuthnRequired 已登记 passkey 的管理员用纯口令登录（40012 流程信号）。
	// 与 ErrTotpRequired 同语义：口令已经对、只缺第二步 ⇒ 不计失败，
	// 否则「开闸」会把口令爆破伪装成 passkey 流程提示并锁死管理员。
	ErrWebAuthnRequired = errors.New("该账号已启用 Passkey 登录，请使用 Passkey 或提供二次验证码")
	// ErrWebAuthnInvalid 挑战过期/断言不合法/凭据不符（400）。
	// 库的协议细节只进服务端日志，对外统一文案（core#114 A6b 同精神）。
	ErrWebAuthnInvalid = errors.New("Passkey 验证失败")
	// ErrWebAuthnInternal 登录链路内部故障（DB/签发）标记，handler 走脱敏出网。
	ErrWebAuthnInternal = errors.New("Passkey 内部错误")
)

// WebAuthnRP 依赖方参数：handler 从配置 + 请求头解析后注入，service 只消费。
type WebAuthnRP struct {
	// RPID 依赖方 ID（不含 scheme/port）——认证器据此算 rpIdHash，服务端逐字节比对。
	RPID string
	// Origins 允许的来源（完整 origin）。
	Origins []string
	// DisplayName 认证器上展示的依赖方名。
	DisplayName string
}

// ResolveWebAuthnRP 推导 RP 参数：rp_origins 非空 = **严格白名单**（运维钉死，
// 不再并入推导值）；为空 = 按请求现场推导（scheme://host ∪ 浏览器 Origin 头）。
//
// RPID 是安全锚：默认取请求 Host 去端口（可用 security.webauthn.rp_id 钉死）。
// 推导模式下 TLS 在反代终结时后端看到的 scheme 是 http，只有并入浏览器实际
// Origin 头才能过库校验；跨站页面的 RP ID 与本站不同 ⇒ rpIdHash 仍是硬兜底
// （推导 Origin 不构成跨站放行面）。
func ResolveWebAuthnRP(cfg conf.WebAuthnConfig, host, scheme, forwardedProto, originHeader string) WebAuthnRP {
	rpid := strings.TrimSpace(cfg.RPID)
	if rpid == "" {
		rpid = webAuthnStripPort(strings.TrimSpace(host))
	}
	display := strings.TrimSpace(cfg.DisplayName)
	if display == "" {
		display = "PigeonBox"
	}
	proto := ""
	if xfp := strings.TrimSpace(forwardedProto); xfp != "" {
		proto = strings.TrimSpace(strings.Split(xfp, ",")[0])
	}
	if proto == "" {
		proto = strings.TrimSpace(scheme)
	}
	if proto == "" {
		proto = "http"
	}
	if proto != "http" && proto != "https" {
		proto = "http" // 非法 X-Forwarded-Proto 不外溢（Origin 头仍兜底）
	}
	origins := make([]string, 0, len(cfg.RPOrigins)+2)
	seen := make(map[string]bool, len(cfg.RPOrigins)+2)
	add := func(o string) {
		o = strings.TrimSpace(o)
		if o == "" || seen[o] {
			return
		}
		seen[o] = true
		origins = append(origins, o)
	}
	if len(cfg.RPOrigins) > 0 {
		// 钉死模式：只认配置白名单（严格；含 scheme/port 的完整 origin）
		for _, o := range cfg.RPOrigins {
			add(o)
		}
	} else {
		// 推导模式：配置 ∪ 推导 origin ∪ 浏览器 Origin 头
		if h := strings.TrimSpace(host); h != "" {
			add(proto + "://" + h)
		}
		add(originHeader)
	}
	return WebAuthnRP{RPID: rpid, Origins: origins, DisplayName: display}
}

// webAuthnStripPort 去端口（RP ID 规范不含端口）；解析失败原样返回。
func webAuthnStripPort(host string) string {
	if host == "" {
		return host
	}
	if h, _, err := net.SplitHostPort(host); err == nil && h != "" {
		return h
	}
	return host
}

// webAuthnCeremonyKind ceremony 用途（take 时校验，防登记挑战被登录步消费）。
type webAuthnCeremonyKind uint8

const (
	webAuthnCeremonyRegister webAuthnCeremonyKind = iota + 1
	webAuthnCeremonyLogin
)

// webAuthnCeremony 一次 Begin→Finish 之间的服务端状态。
type webAuthnCeremony struct {
	kind webAuthnCeremonyKind
	// userID 登记 = 发起人；登录 = 目标管理员；0 = 诱饵挑战（用户名不存在/
	// 未登记 passkey/非管理员）⇒ 验证步一律拒绝，不给用户名枚举预言机。
	userID  uint
	session webauthn.SessionData
	expires time.Time
}

// webAuthnCeremonyStore 进程内 challenge 存取：一次性消费 + TTL。
type webAuthnCeremonyStore struct {
	mu sync.Mutex
	m  map[string]*webAuthnCeremony
}

// webAuthnCeremonyTTL challenge 有效期（WebAuthn timeout 常见 60s，放宽到 5min
// 容忍慢手机/系统弹窗；过期即作废，用户重取即可）。
const webAuthnCeremonyTTL = 5 * time.Minute

func (s *webAuthnCeremonyStore) put(challenge string, c *webAuthnCeremony) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.m {
		if v.expires.Before(now) {
			delete(s.m, k)
		}
	}
	s.m[challenge] = c
}

// take 一次性消费：命中即删（无论后续验证成败）——同一 challenge 不给第二次机会。
func (s *webAuthnCeremonyStore) take(challenge string, kind webAuthnCeremonyKind) (*webAuthnCeremony, bool) {
	if challenge == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.m[challenge]
	if !ok {
		return nil, false
	}
	delete(s.m, challenge)
	if c.kind != kind || time.Now().After(c.expires) {
		return nil, false
	}
	return c, true
}

// webAuthnCeremonies 包级单例（admin 面单进程持有；多副本不共享——见文件头）。
var webAuthnCeremonies = &webAuthnCeremonyStore{m: make(map[string]*webAuthnCeremony)}

// webAuthnUser 库的 User 适配：model.User + 该用户全部凭证。
type webAuthnUser struct {
	user  *model.User
	creds []webauthn.Credential
}

func (u *webAuthnUser) WebAuthnID() []byte {
	// 稳定、非空、≤64 字节：users.id 十进制串（库里以 user_id 归属，
	// 断言的 userHandle 也用它回查——与 discoverable 流兼容）。
	return []byte(strconv.FormatUint(uint64(u.user.ID), 10))
}

func (u *webAuthnUser) WebAuthnName() string { return u.user.Username }

func (u *webAuthnUser) WebAuthnDisplayName() string {
	if n := strings.TrimSpace(u.user.Nickname); n != "" {
		return n
	}
	return u.user.Username
}

func (u *webAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// newWebAuthnFor 按 RP 参数构造库实例（每次请求构造：RP 参数可能随请求头变化）。
func newWebAuthnFor(rp WebAuthnRP) (*webauthn.WebAuthn, error) {
	if strings.TrimSpace(rp.RPID) == "" {
		return nil, fmt.Errorf("%w: 依赖方 ID 缺失（请求 Host 为空？可配置 security.webauthn.rp_id）", ErrWebAuthnInvalid)
	}
	return webauthn.New(&webauthn.Config{
		RPID:          rp.RPID,
		RPDisplayName: rp.DisplayName,
		RPOrigins:     rp.Origins,
	})
}

// webAuthnUserOf 装配某用户的 User 适配（含凭证装载）。
func (s *Service) webAuthnUserOf(ctx context.Context, u *model.User) (*webAuthnUser, error) {
	rows, err := s.webauthnRepo.ListByUser(ctx, u.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: 凭证查询失败: %w", ErrWebAuthnInternal, err)
	}
	creds := make([]webauthn.Credential, 0, len(rows))
	for _, row := range rows {
		cred, err := webAuthnCredentialFromRow(row)
		if err != nil {
			return nil, err
		}
		creds = append(creds, cred)
	}
	return &webAuthnUser{user: u, creds: creds}, nil
}

// webAuthnCredentialFromRow 库凭证 ← 落库行。
func webAuthnCredentialFromRow(row *model.WebAuthnCredential) (webauthn.Credential, error) {
	id, err := base64.RawURLEncoding.DecodeString(row.CredentialID)
	if err != nil || len(id) == 0 {
		return webauthn.Credential{}, fmt.Errorf("%w: 凭证 ID 损坏", ErrWebAuthnInternal)
	}
	cred := webauthn.Credential{
		ID:        id,
		PublicKey: row.PublicKey,
		Authenticator: webauthn.Authenticator{
			SignCount: row.SignCount,
		},
	}
	if row.AAGUID != "" {
		if b, err := hex.DecodeString(row.AAGUID); err == nil {
			cred.Authenticator.AAGUID = b
		}
	}
	if row.Transports != "" {
		for _, t := range strings.Split(row.Transports, ",") {
			if t = strings.TrimSpace(t); t != "" {
				cred.Transport = append(cred.Transport, protocol.AuthenticatorTransport(t))
			}
		}
	}
	return cred, nil
}

// webAuthnJSONRequest 把客户端提交的凭证 JSON 包成库要求的 *http.Request。
// （库只读 Body——tests 同款构造；Header 置空 map 防 nil 写入）。
func webAuthnJSONRequest(body []byte) *http.Request {
	return &http.Request{
		Body:   io.NopCloser(bytes.NewReader(body)),
		Header: make(http.Header),
	}
}

// ===== 登记流 =====

// WebAuthnRegisterOptions 登记第一步：发 challenge（管理员 JWT 保护）。
//
// 已登记的凭证进 excludeCredentials（同一张 passkey 不重复登记）；
// challenge 进程内一次性存取，5 分钟 TTL。
func (s *Service) WebAuthnRegisterOptions(ctx context.Context, userID uint, rp WebAuthnRP) (*protocol.CredentialCreation, error) {
	u, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("%w: 用户不存在: %w", ErrWebAuthnInvalid, err)
	}
	if u.Role != "admin" || u.Status != "active" {
		return nil, fmt.Errorf("%w: 仅在职管理员可登记 Passkey", ErrWebAuthnInvalid)
	}
	w, err := newWebAuthnFor(rp)
	if err != nil {
		return nil, err
	}
	wu, err := s.webAuthnUserOf(ctx, u)
	if err != nil {
		return nil, err
	}
	creation, session, err := w.BeginRegistration(wu,
		webauthn.WithExclusions(webauthn.Credentials(wu.creds).CredentialDescriptors()))
	if err != nil {
		logger.CtxWarn(ctx, "webauthn registration begin failed", "user_id", userID, "err", err)
		return nil, fmt.Errorf("%w: 挑战生成失败", ErrWebAuthnInvalid)
	}
	webAuthnCeremonies.put(session.Challenge, &webAuthnCeremony{
		kind:    webAuthnCeremonyRegister,
		userID:  userID,
		session: *session,
		expires: time.Now().Add(webAuthnCeremonyTTL),
	})
	return creation, nil
}

// WebAuthnRegisterVerify 登记第二步：校验 attestation 并落库。
//
// challenge 一次性消费（验证成败都作废）；userID 必须与发起 challenge 的
// 一致（防「我领的挑战你来验」串号）。
func (s *Service) WebAuthnRegisterVerify(ctx context.Context, userID uint, rp WebAuthnRP, challenge string, credentialJSON []byte, name string) (*model.WebAuthnCredential, error) {
	cer, ok := webAuthnCeremonies.take(challenge, webAuthnCeremonyRegister)
	if !ok || cer.userID != userID {
		return nil, fmt.Errorf("%w: 登记挑战无效或已过期", ErrWebAuthnInvalid)
	}
	u, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("%w: 用户不存在: %w", ErrWebAuthnInvalid, err)
	}
	if u.Role != "admin" || u.Status != "active" {
		return nil, fmt.Errorf("%w: 仅在职管理员可登记 Passkey", ErrWebAuthnInvalid)
	}
	w, err := newWebAuthnFor(rp)
	if err != nil {
		return nil, err
	}
	wu, err := s.webAuthnUserOf(ctx, u)
	if err != nil {
		return nil, err
	}
	cred, err := w.FinishRegistration(wu, cer.session, webAuthnJSONRequest(credentialJSON))
	if err != nil {
		logger.CtxWarn(ctx, "webauthn registration verify failed", "user_id", userID, "err", err)
		return nil, fmt.Errorf("%w: 断言校验失败", ErrWebAuthnInvalid)
	}
	credID := base64.RawURLEncoding.EncodeToString(cred.ID)
	if _, err := s.webauthnRepo.GetByCredentialID(ctx, credID); err == nil {
		return nil, fmt.Errorf("%w: 该 Passkey 已登记", ErrWebAuthnInvalid)
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: 凭证查重失败: %w", ErrWebAuthnInternal, err)
	}
	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	label := strings.TrimSpace(name)
	if label == "" {
		label = "Passkey " + credID[:minInt(8, len(credID))]
	}
	row := &model.WebAuthnCredential{
		UserID:       userID,
		CredentialID: credID,
		PublicKey:    cred.PublicKey,
		SignCount:    cred.Authenticator.SignCount,
		Transports:   strings.Join(transports, ","),
		AAGUID:       hex.EncodeToString(cred.Authenticator.AAGUID),
		Name:         label,
	}
	if err := s.webauthnRepo.Create(ctx, row); err != nil {
		return nil, fmt.Errorf("%w: 凭证落库失败: %w", ErrWebAuthnInternal, err)
	}
	logger.CtxInfo(ctx, "webauthn credential registered", "user_id", userID, "cred_id", credID, "name", label)
	return row, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ===== 登录流 =====

// WebAuthnLoginOptions 登录第一步（公开面）：按用户名发断言挑战。
//
// 防枚举：目标不存在 / 非在职管理员 / 未登记 passkey 时发**同形诱饵**挑战
// （allowCredentials 填随机 ID，形状与真挑战一致），验证步因 userID=0 一律拒绝。
func (s *Service) WebAuthnLoginOptions(ctx context.Context, username string, rp WebAuthnRP) (*protocol.CredentialAssertion, error) {
	w, err := newWebAuthnFor(rp)
	if err != nil {
		return nil, err
	}
	target, err := s.resolveWebAuthnLoginTarget(ctx, username)
	if err != nil {
		return nil, err
	}
	if target == nil {
		// 诱饵挑战
		assertion, session, err := w.BeginDiscoverableLogin()
		if err != nil {
			return nil, fmt.Errorf("%w: 挑战生成失败", ErrWebAuthnInvalid)
		}
		dummy := make([]byte, 32)
		if _, err := rand.Read(dummy); err != nil {
			return nil, fmt.Errorf("%w: 挑战生成失败", ErrWebAuthnInvalid)
		}
		assertion.Response.AllowedCredentials = []protocol.CredentialDescriptor{{
			Type:         protocol.PublicKeyCredentialType,
			CredentialID: protocol.URLEncodedBase64(dummy),
		}}
		webAuthnCeremonies.put(session.Challenge, &webAuthnCeremony{
			kind:    webAuthnCeremonyLogin,
			userID:  0, // 诱饵
			session: *session,
			expires: time.Now().Add(webAuthnCeremonyTTL),
		})
		return assertion, nil
	}
	assertion, session, err := w.BeginLogin(target.wu)
	if err != nil {
		logger.CtxWarn(ctx, "webauthn login begin failed", "username", username, "err", err)
		return nil, fmt.Errorf("%w: 挑战生成失败", ErrWebAuthnInvalid)
	}
	webAuthnCeremonies.put(session.Challenge, &webAuthnCeremony{
		kind:    webAuthnCeremonyLogin,
		userID:  target.user.ID,
		session: *session,
		expires: time.Now().Add(webAuthnCeremonyTTL),
	})
	return assertion, nil
}

// webAuthnLoginTarget 在职管理员 + 已登记 passkey；不满足返回 nil（发诱饵）。
type webAuthnLoginTarget struct {
	user *model.User
	wu   *webAuthnUser
}

func (s *Service) resolveWebAuthnLoginTarget(ctx context.Context, username string) (*webAuthnLoginTarget, error) {
	u, err := s.userRepo.GetByUsername(ctx, strings.TrimSpace(username))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: 用户查询失败: %w", ErrWebAuthnInternal, err)
	}
	if u.Role != "admin" || u.Status != "active" {
		return nil, nil
	}
	wu, err := s.webAuthnUserOf(ctx, u)
	if err != nil {
		return nil, err
	}
	if len(wu.creds) == 0 {
		return nil, nil
	}
	return &webAuthnLoginTarget{user: u, wu: wu}, nil
}

// WebAuthnLoginVerify 登录第二步（公开面）：校验断言并签发管理员 JWT。
//
// challenge 一次性消费；诱饵挑战（userID=0）直接拒绝；
// 成功后回写签名计数（克隆判据的持久面）与最近使用时间。
func (s *Service) WebAuthnLoginVerify(ctx context.Context, rp WebAuthnRP, challenge string, credentialJSON []byte) (string, error) {
	cer, ok := webAuthnCeremonies.take(challenge, webAuthnCeremonyLogin)
	if !ok {
		return "", fmt.Errorf("%w: 登录挑战无效或已过期", ErrWebAuthnInvalid)
	}
	if cer.userID == 0 {
		return "", fmt.Errorf("%w: 登录挑战无效或已过期", ErrWebAuthnInvalid)
	}
	u, err := s.userRepo.GetByID(ctx, cer.userID)
	if err != nil || u.Role != "admin" || u.Status != "active" {
		return "", fmt.Errorf("%w: 登录挑战无效或已过期", ErrWebAuthnInvalid)
	}
	wu, err := s.webAuthnUserOf(ctx, u)
	if err != nil {
		return "", err
	}
	w, err := newWebAuthnFor(rp)
	if err != nil {
		return "", err
	}
	cred, err := w.FinishLogin(wu, cer.session, webAuthnJSONRequest(credentialJSON))
	if err != nil {
		logger.CtxWarn(ctx, "webauthn login verify failed", "user_id", u.ID, "err", err)
		return "", fmt.Errorf("%w: 断言校验失败", ErrWebAuthnInvalid)
	}
	now := time.Now()
	if err := s.webauthnRepo.UpdateAfterLogin(ctx,
		base64.RawURLEncoding.EncodeToString(cred.ID),
		cred.Authenticator.SignCount, now); err != nil {
		// 计数器回写失败不阻断登录（下次仍会比对库里旧值）
		logger.CtxWarn(ctx, "webauthn sign count update failed", "user_id", u.ID, "err", err)
	}
	token, err := auth.GenerateToken(u.ID, u.Username, "admin")
	if err != nil {
		return "", fmt.Errorf("%w: 签发令牌失败: %w", ErrWebAuthnInternal, err)
	}
	u.LastLoginAt = &now
	_ = s.userRepo.Update(ctx, u)
	logger.CtxInfo(ctx, "webauthn admin login ok", "user_id", u.ID, "cred_id", cred.ID)
	return token, nil
}

// ===== 登录闸（挂接 /admin/login） =====

// webAuthnGateEnabled 读 security.require_admin_webauthn（默认关；快照缺席=关）。
func webAuthnGateEnabled() bool {
	snap := conf.Snapshot()
	return snap != nil && snap.Security.RequireAdminWebAuthn
}
