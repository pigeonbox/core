package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
