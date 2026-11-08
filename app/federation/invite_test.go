package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pigeonbox/core/conf"
)

// TestInviteMailboxInbox 收件箱:格式校验/去重/满箱/过期/认领出箱。
//
// 2026-10-22 自直传包测试搬迁（信封管理实现随 Wave B B0 波搬入
// 本包）：断言口径与搬迁前一致，仅被测对象由直传包的 supervisor 改为 *InviteMailbox。
func TestInviteMailboxInbox(t *testing.T) {
	base := time.Now()
	now := base
	m := NewInviteMailbox(func() time.Time { return now })

	// 格式校验
	if _, err := m.AddInvite("bad-code", "a", ""); err == nil {
		t.Fatal("坏口令应拒")
	}
	// 入箱 + 归一化(小写/无连字符输入)
	inv, err := m.AddInvite("KJ8YWCYZE249", "发送站A", "note\n\x01x")
	if err != nil {
		t.Fatalf("入箱失败: %v", err)
	}
	if inv.Code != "KJ8Y-WCYZ-E249" {
		t.Fatalf("口令归一化错: %q", inv.Code)
	}
	if strings.ContainsAny(inv.Note, "\n\x01") {
		t.Fatalf("note 未消毒: %q", inv.Note)
	}
	// 同码去重
	if n := len(m.ListInvites()); n != 1 {
		t.Fatalf("去重失败: %d", n)
	}
	// 快照字段与过期倒计时
	items := m.ListInvites()
	if len(items) != 1 {
		t.Fatalf("列快照数不对: %d", len(items))
	}
	if items[0].ID != inv.ID || items[0].Code != inv.Code || items[0].FromName != "发送站A" {
		t.Fatalf("快照字段不符: %+v", items[0])
	}
	if items[0].ExpiresIn != int64(inviteTTL.Seconds()) {
		t.Fatalf("过期倒计时不符: %d", items[0].ExpiresIn)
	}
	// 认领出箱
	got, err := m.TakeInvite(inv.ID)
	if err != nil || got.Code != "KJ8Y-WCYZ-E249" {
		t.Fatalf("认领失败: %+v %v", got, err)
	}
	if _, err := m.TakeInvite(inv.ID); err == nil {
		t.Fatal("重复认领应拒")
	}
	// 满箱
	for i := 0; i < 20; i++ {
		code := fmt.Sprintf("%04X-%04X-%04X", i, i, i)
		if _, err := m.AddInvite(code, "", ""); err != nil {
			t.Fatalf("第%d 个入箱失败: %v", i, err)
		}
	}
	if _, err := m.AddInvite("FFFF-FFFF-FFFF", "", ""); err == nil {
		t.Fatal("满箱应拒")
	}
	// 过期清扫(时钟拨快)
	now = base.Add(11 * time.Minute)
	if n := len(m.ListInvites()); n != 0 {
		t.Fatalf("超 TTL 应清空: %d", n)
	}
}

// TestInviteMailboxListOrder 列表新到在前(前端渲染顺序)。
func TestInviteMailboxListOrder(t *testing.T) {
	m := NewInviteMailbox(nil)
	first, err := m.AddInvite("AAAA-AAAA-AAAA", "A", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.AddInvite("BBBB-BBBB-BBBB", "B", "")
	if err != nil {
		t.Fatal(err)
	}
	got := m.ListInvites()
	if len(got) != 2 {
		t.Fatalf("应有 2 条: %d", len(got))
	}
	if got[0].ID != second.ID || got[1].ID != first.ID {
		t.Fatalf("顺序不符(新到在前): %+v", got)
	}
}

// TestInviteMailbox_ExpiresIn 已出箱信封的剩余有效期方向回归（2026-10-26 审计 P1）。
//
// 缺陷：ExpiresIn 写成「已过时长 - TTL」⇒ 恒为负 ⇒ 被钳零逻辑吞成 0 ⇒
// 已出箱信封（HTTP POST /admin/p2p/invites/accept、MCP p2p_accept 都用它）
// 恒返回「剩余 0 秒」⇒ PigeonBeam 侧判定信封已到期（10 分钟窗口形同虚设）。
// 同文件 ListInvites 用的是相反且正确的方向 ⇒ 两处口径漂移。
//
// 断言必须能变红（负对照）：把 remainingSeconds 里的减法顺序改回
// 「now - (ReceivedAt+TTL)」即失败。
func TestInviteMailbox_ExpiresIn(t *testing.T) {
	base := time.Now()
	now := base
	m := NewInviteMailbox(func() time.Time { return now })

	inv, err := m.AddInvite("AAAA-BBBB-CCCC", "发送站A", "")
	if err != nil {
		t.Fatalf("入箱失败: %v", err)
	}

	// 刚入箱 ⇒ 剩余 = TTL（不是 0）
	if got := m.ExpiresIn(inv); got != int64(inviteTTL.Seconds()) {
		t.Fatalf("刚入箱的剩余有效期不对: got=%d want=%d", got, int64(inviteTTL.Seconds()))
	}

	// 时钟推进 ⇒ 剩余递减（方向正确的第二证据：反向实现会得到负值→0）
	now = base.Add(inviteTTL / 2)
	want := int64((inviteTTL - inviteTTL/2).Seconds())
	if got := m.ExpiresIn(inv); got != want {
		t.Fatalf("推进时钟后的剩余有效期不对: got=%d want=%d", got, want)
	}

	// 拨过 TTL ⇒ 0（钳零仍然生效）
	now = base.Add(inviteTTL + time.Second)
	if got := m.ExpiresIn(inv); got != 0 {
		t.Fatalf("过期后应为 0: got=%d", got)
	}

	// 与 ListInvites 快照口径一致（两处同源，不再漂移）
	now = base
	items := m.ListInvites()
	if len(items) != 1 {
		t.Fatalf("列快照数不对: %d", len(items))
	}
	if items[0].ExpiresIn != m.ExpiresIn(inv) {
		t.Fatalf("快照与 ExpiresIn 口径不一致: %d vs %d", items[0].ExpiresIn, m.ExpiresIn(inv))
	}
}

// TestPushInvite 推送:目标路径 + 口令归一 + 附言消毒（信封的第四个动作）。
func TestPushInvite(t *testing.T) {
	// 私网目标需显式放行（SSRF 守卫与 registry_url 同策略）
	restore := allowPrivateNetworks(t, true)
	defer restore()

	var gotPath, gotMethod string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := PushInvite(context.Background(), srv.URL, "kj8ywcyze249", "发送站A", "note\n\x01x"); err != nil {
		t.Fatalf("推送失败: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/p2p/invite" {
		t.Fatalf("推送目标不符: %s %s", gotMethod, gotPath)
	}
	if gotBody["code"] != "KJ8Y-WCYZ-E249" {
		t.Fatalf("推送口令未归一: %q", gotBody["code"])
	}
	if gotBody["from_name"] != "发送站A" {
		t.Fatalf("推送 from_name 不符: %q", gotBody["from_name"])
	}
	if strings.ContainsAny(gotBody["note"], "\n\x01") {
		t.Fatalf("推送 note 未消毒: %q", gotBody["note"])
	}
}

// TestPushInviteErrors 推送失败语义:空地址 / SSRF 拦截 / 非 200。
func TestPushInviteErrors(t *testing.T) {
	restore := allowPrivateNetworks(t, true)
	defer restore()

	if err := PushInvite(context.Background(), "  ", "AAAA-AAAA-AAAA", "", ""); err == nil ||
		!strings.Contains(err.Error(), "目标实例地址不能为空") {
		t.Fatalf("空地址应拒: %v", err)
	}

	// SSRF 拦截:私网目标且未放行
	restore2 := allowPrivateNetworks(t, false)
	defer restore2()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	err := PushInvite(context.Background(), srv.URL, "AAAA-AAAA-AAAA", "", "")
	if err == nil || !strings.Contains(err.Error(), "目标地址校验失败") {
		t.Fatalf("私网目标应被 SSRF 拦截: %v", err)
	}

	// 非 200
	restore3 := allowPrivateNetworks(t, true)
	defer restore3()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer bad.Close()
	err = PushInvite(context.Background(), bad.URL, "AAAA-AAAA-AAAA", "", "")
	if err == nil || !strings.Contains(err.Error(), "目标实例返回 502") {
		t.Fatalf("非 200 应报错: %v", err)
	}
}

// allowPrivateNetworks 临时开关 SSRF 私网放行（返回恢复函数）。
func allowPrivateNetworks(t *testing.T, allow bool) func() {
	t.Helper()
	old := conf.GetGlobalConfig()
	cfg := &conf.AppConfiguration{}
	cfg.Security.SSRF.AllowPrivateNetworks = allow
	conf.SetGlobalConfig(cfg)
	return func() { conf.SetGlobalConfig(old) }
}

// TestPushInviteToTarget_PrivatePeerPerPeerGranularity 同步对端的 SSRF 是
// **对端粒度**（core#32）：私网对端仅当该条目 allow_private=true 才放行，
// 不开全局 security.ssrf.allow_private_networks 后门。
//
// 判据形状：
//  1. allow_private=false ⇒ SSRF 拒，且**一个出站请求都不发**（拒在请求前）；
//  2. allow_private=true ⇒ 放行并成功送达（私网自托管对端的正常形态）；
//  3. 全局开关全程保持关闭（开了就等于全局后门，本测试的判据会失去意义）。
//
// ⚠️ 负对照（改错转红）：把 ② 的放行判据改成无条件 ValidateEndpointURL
// （即丢掉对端粒度）⇒ ② 转红；把 ① 改成无条件放行 ⇒ ① 的拦截断言转红。
func TestPushInviteToTarget_PrivatePeerPerPeerGranularity(t *testing.T) {
	restore := allowPrivateNetworks(t, false)
	defer restore()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// ① 未放行的私网对端：拒，且不出站
	err := PushInviteToTarget(context.Background(),
		conf.FederationSyncTarget{URL: srv.URL, AllowPrivate: false},
		"AAAA-AAAA-AAAA", "", "")
	if err == nil || !strings.Contains(err.Error(), "目标地址校验失败") {
		t.Fatalf("私网对端未放行应被 SSRF 拦截: %v", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("被拒后仍有出站请求: hits=%d，期望 0（拒在请求前）", got)
	}

	// ② 对端粒度放行：成功送达
	if err := PushInviteToTarget(context.Background(),
		conf.FederationSyncTarget{URL: srv.URL, AllowPrivate: true},
		"AAAA-AAAA-AAAA", "发送站A", ""); err != nil {
		t.Fatalf("allow_private=true 的私网对端应放行: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("放行后应恰好送达一次: hits=%d", got)
	}
}

// ---- core#86：随机源失败的错误上抛（原实现吞错 ⇒ 静默发弱信封 ID） ----

// boomReader 必然失败的随机源注入桩（core#86 用例专用；federation_test.go 的
// randomNonce 用例同包共用）。
type boomReader struct{}

func (boomReader) Read(p []byte) (int, error) { return 0, errors.New("boom: 熵源故障") }

// TestNewEnvelopeID_RandFailurePropagates（core#86）：随机源失败必须显式上抛、
// 绝不入箱弱 ID——原实现吞错 ⇒ 熵源故障时恒发全 0 重复 ID ⇒ 收件箱条目互相
// 覆盖（与 qrcode 审计 A5 同款缺陷）。
//
// 负对照：改回吞错形态（忽略 err 用半截产物）⇒ err 断言转红；弱 ID 入箱 ⇒
// 收件箱条数断言转红——两条判据各自锁死一种复活形态。
func TestNewEnvelopeID_RandFailurePropagates(t *testing.T) {
	m := NewInviteMailbox(nil)

	// 基线：默认随机源可用 ⇒ 正常入箱，ID 为 16 hex（证明判据走的是注入路径，非恒红）
	inv, err := m.AddInvite("KJ8YWCYZE249", "a", "")
	if err != nil {
		t.Fatalf("默认源下入箱不应失败: %v", err)
	}
	if len(inv.ID) != 16 {
		t.Fatalf("信封 ID 长度不对: %q", inv.ID)
	}

	// 注入必然失败的 reader ⇒ AddInvite 报错、零新增入箱
	orig := randSource
	randSource = boomReader{}
	defer func() { randSource = orig }()

	got, err := m.AddInvite("KJ8YWCYZE250", "b", "")
	if err == nil {
		t.Fatal("随机源失败必须上抛（吞错形态会在这里假绿）")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("错误链必须携带注入的失败原因: %v", err)
	}
	if got.ID != "" {
		t.Fatalf("错误路径不得返回半截产物: %q", got.ID)
	}
	if n := len(m.ListInvites()); n != 1 {
		t.Fatalf("弱 ID 不得入箱：收件箱应仍为基线的 1 条，实际 %d 条", n)
	}
}
