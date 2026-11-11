package outbound

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/security"
)

// TestNew_Reuse 同 target 两次 New 返回同一实例（连接池复用）；不同 target 必须
// 各自独立实例（策略不同，绝不能串）；并发取用也收敛到同一实例。
func TestNew_Reuse(t *testing.T) {
	a := New(TargetNotify)
	b := New(TargetNotify)
	if a != b {
		t.Fatal("同 target 两次 New 应返回同一 *http.Client 实例")
	}
	if New(TargetOIDC) == a {
		t.Fatal("不同 target 必须是不同实例（策略不同）")
	}
	for _, tg := range []Target{TargetNotify, TargetOIDC, TargetFederation, TargetFederationDelivery} {
		if New(tg) == nil {
			t.Fatalf("枚举 target %q 取到的客户端为 nil", string(tg))
		}
	}
	// 并发取用：全部收敛到同一指针（注册表缓存的并发安全）。
	const n = 32
	clients := make(chan *http.Client, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			clients <- New(TargetFederation)
		}()
	}
	wg.Wait()
	close(clients)
	for c := range clients {
		if c != New(TargetFederation) {
			t.Fatal("并发 New 返回了不同实例")
		}
	}
}

// TestNew_UnknownTarget_Panics 未知 target 必须 fail-fast panic。
//
// 理由：调用点全部使用 Target* 编译期常量，写错只能是编程 bug；panic 发生在
// 启动期的构造现场（参照 regexp.MustCompile 语义），而静默回落默认客户端会把
// 策略错配（超时/重定向守卫全错）藏到生产流量里才暴露——fail-fast 显著优于后者。
func TestNew_UnknownTarget_Panics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("未知 target 必须 panic（fail-fast），不应返回客户端")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "bogus") {
			t.Fatalf("panic 消息应含非法 target 名，得到: %v", r)
		}
	}()
	New(Target("bogus"))
}

// TestObserve httptest 端到端：注入观测出口，断言三种 outcome 均被正确上报
// （ok=2xx / status_error=非 2xx / transport_error=传输层失败）且 target 正确、
// 耗时非负。
func TestObserve(t *testing.T) {
	record := func(t *testing.T) (*[]string, *http.Client) {
		var (
			mu    sync.Mutex
			calls []string
		)
		obs := func(tg Target, o Outcome, d time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, fmt.Sprintf("%s=%s", string(tg), string(o)))
			if d < 0 {
				t.Errorf("观测耗时为负: %v", d)
			}
		}
		return &calls, newObservedClient(TargetNotify, obs)
	}

	t.Run("ok_2xx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		calls, client := record(t)
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("请求不应失败: %v", err)
		}
		_ = resp.Body.Close()
		if got, want := *calls, []string{"notify=ok"}; !equalCalls(got, want) {
			t.Fatalf("观测记录 = %v, 期望 %v", got, want)
		}
	})

	t.Run("status_error_5xx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		calls, client := record(t)
		resp, err := client.Get(srv.URL) // 5xx 不产生 err
		if err != nil {
			t.Fatalf("5xx 应返回响应而非错误: %v", err)
		}
		_ = resp.Body.Close()
		if got, want := *calls, []string{"notify=status_error"}; !equalCalls(got, want) {
			t.Fatalf("观测记录 = %v, 期望 %v", got, want)
		}
	})

	t.Run("transport_error_refused", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		url := srv.URL
		srv.Close() // 关闭后连接拒绝 ⇒ 传输层失败

		calls, client := record(t)
		if _, err := client.Get(url); err == nil {
			t.Fatal("对已关闭服务端请求应失败")
		}
		if got, want := *calls, []string{"notify=transport_error"}; !equalCalls(got, want) {
			t.Fatalf("观测记录 = %v, 期望 %v", got, want)
		}
	})
}

// TestTimeouts 超时基准在注册表统一治理：oidc == DefaultTimeout（基准本尊）；
// notify/federation 的 5s 与 delivery 的 0 是**迁移保留**的原值（wire 零变化，
// 出处见 policies 注释）——基准值本身钉死，防无意漂移。
func TestTimeouts(t *testing.T) {
	if DefaultTimeout != 15*time.Second {
		t.Fatalf("DefaultTimeout 漂移: %v（基准应为 15s）", DefaultTimeout)
	}
	if got := New(TargetOIDC).Timeout; got != DefaultTimeout {
		t.Fatalf("TargetOIDC.Timeout = %v, 期望 DefaultTimeout %v", got, DefaultTimeout)
	}
	if got := New(TargetNotify).Timeout; got != 5*time.Second {
		t.Fatalf("TargetNotify.Timeout = %v, 期望迁移保留的 5s", got)
	}
	if got := New(TargetFederation).Timeout; got != 5*time.Second {
		t.Fatalf("TargetFederation.Timeout = %v, 期望迁移保留的 5s", got)
	}
	if got := New(TargetFederationDelivery).Timeout; got != 0 {
		t.Fatalf("TargetFederationDelivery.Timeout = %v, 期望 0（ctx 截止治理，原 DeliveryClient 语义）", got)
	}
}

// TestNotifyRedirectGuard SSRF 重定向守卫随收编迁入注册表后语义不变（2026-10-05
// 审计 P3 的行为锁）：重定向到私网地址（httptest=127.0.0.1，测试环境未放行
// security.ssrf.allow_private_networks）必须被拒。
// 观测口径：守卫在 CheckRedirect（transport 之上）拒绝，302 那一跳本身已过
// RoundTrip ⇒ 计为 status_error，不是 transport_error。
func TestNotifyRedirectGuard(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redirector.Close()

	var outcome Outcome
	client := newObservedClient(TargetNotify, func(_ Target, o Outcome, _ time.Duration) {
		outcome = o
	})
	resp, err := client.Get(redirector.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("私网重定向必须被 SSRF 守卫拒绝")
	}
	if !strings.Contains(err.Error(), "重定向目标被拒绝") {
		t.Fatalf("错误应来自 webhook 重定向守卫, 得到: %v", err)
	}
	if outcome != OutcomeStatusErr {
		t.Fatalf("被拒重定向的 302 跳应计为 status_error, 得到 %q", outcome)
	}
}

// TestFederationDeliveryPolicy 跨站投递的两项原语义随收编保留：
// ① 禁跟随重定向（302 按原响应返回，ErrUseLastResponse）——对端 302 到内网地址
// 不能借本站出站面绕过 SSRF 判定；② 无客户端级超时（流式上传，ctx 截止治理，
// TestTimeouts 已锁）。
func TestFederationDeliveryPolicy(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redirector.Close()

	var outcome Outcome
	client := newObservedClient(TargetFederationDelivery, func(_ Target, o Outcome, _ time.Duration) {
		outcome = o
	})
	resp, err := client.Get(redirector.URL)
	if err != nil {
		t.Fatalf("ErrUseLastResponse 口径下 302 不产生错误: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("应返回 302 原响应（不跟随）, 得到 %d", resp.StatusCode)
	}
	if outcome != OutcomeStatusErr {
		t.Fatalf("3xx 应计为 status_error, 得到 %q", outcome)
	}
}

// TestFederationFollowsRedirect registry 目标在守卫放行时仍跟随重定向。
//
// ⚠️ core#140 语义变更（有意）：原「Go 默认盲跟 10 跳」收紧为「逐跳过
// ssrfRedirectGuard 才跟随」——跳转目标过校验则照常跟随（本用例），指向
// 私网/环回则拒绝（TestFederationRegistryRedirectGuard）。测试环境两跳都是
// 环回 ⇒ 显式放行全局 security.ssrf.allow_private_networks，验证守卫不误伤
// 合法跳转。
func TestFederationFollowsRedirect(t *testing.T) {
	old := conf.GetGlobalConfig()
	cfg := &conf.AppConfiguration{}
	cfg.Security.SSRF.AllowPrivateNetworks = true
	conf.SetGlobalConfig(cfg)
	defer conf.SetGlobalConfig(old)

	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redirector.Close()

	client := newObservedClient(TargetFederation, func(Target, Outcome, time.Duration) {})
	resp, err := client.Get(redirector.URL)
	if err != nil {
		t.Fatalf("守卫放行的重定向应跟随: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应跟随重定向到 200, 得到 %d", resp.StatusCode)
	}
}

// TestOutboundClientExclusivity 机械守卫（core#87 判据）：整个模块里
// `&http.Client{` 只允许出现在注册表内部与**显式豁免清单**，双向对账——
//   - 新增裸构造 ⇒ 红（须走注册表，或连同理由登记豁免）；
//   - 豁免文件被收编/删除后仍留在清单 ⇒ 红（清单必须匹配现实，防陈旧）；
//   - 注册表自身不再构造 ⇒ 红（守卫自身失效自检）。
//
// 豁免条目都是 issue 现状表未枚举、且各有不可机械套 15s 基准的 per-site 策略
// 的构造点（一次性探测的自定义 TLS transport / 可配超时 / 驱动级长传超时），
// 收编决策留步骤 1.5 或单独立项。
func TestOutboundClientExclusivity(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatalf("定位模块根失败: %v", err)
	}

	registryFile := "pkg/outbound/outbound.go"
	exempt := map[string]string{
		"app/storage/service.go":      "probeHTTP/probeWebDAV 一次性管理面探测（自定义 TLS transport MinVersion TLS1.2）——步骤 1.5 候选",
		"app/updatecheck/service.go":  "更新检查器（可配超时 + 禁重定向 + 自带 TTL 缓存）——步骤 1.5 候选",
		"storage/opendal/azblob.go":   "OpenDAL 驱动 5min 长传（大文件语义，不适用 15s 基准）",
		"storage/opendal/hdfs.go":     "OpenDAL 驱动 5min 长传（大文件语义，不适用 15s 基准）",
		"storage/opendal/onedrive.go": "OpenDAL 驱动 15min 长传（大文件语义，不适用 15s 基准）",
	}

	found := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "&http.Client{") {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			found[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描源码树失败: %v", err)
	}

	if !found[registryFile] {
		t.Fatalf("注册表自身未再构造 &http.Client{——守卫判据失效（或注册表被误改）")
	}

	for f := range found {
		if f == registryFile {
			continue
		}
		if _, ok := exempt[f]; !ok {
			t.Errorf("发现未登记的裸 &http.Client{ 构造: %s——请改走 outbound.New(Target*)，或连同理由登记进本守卫豁免清单", f)
		}
	}
	for f := range exempt {
		if !found[f] {
			t.Errorf("豁免清单条目 %s 已不再含裸构造（文件被收编或删除）——请从清单移除该条目", f)
		}
	}
}

// moduleRoot 向上找 go.mod 定位模块根（go test 的工作目录是包目录）。
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("向上未找到 go.mod")
		}
		dir = parent
	}
}

func equalCalls(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestFederationRegistryRedirectGuard（core#140 正判据）：registry 出站
// （TargetFederation）的重定向同挂逐跳 SSRF 复验——302 到环回地址必须被拒，
// 且被拒后第二跳零出站。此前 checkRedirect = nil（Go 默认盲跟 10 跳）＝ core#116
// 两处 http.DefaultClient 的同族 302 绕过面。
//
// 判据为结构性 errors.Is(err, security.ErrEndpointURL)（错误链可判，不匹配文案）。
//
// 负对照（改错转红）：把 policies[TargetFederation].checkRedirect 置回 nil
// ⇒ 本用例转红（客户端盲跟 302：err==nil 且靶子命中 1）。
func TestFederationRegistryRedirectGuard(t *testing.T) {
	var innerHits atomic.Int64
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		innerHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer inner.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL+"/steal", http.StatusFound)
	}))
	defer redirector.Close()

	client := newObservedClient(TargetFederation, func(Target, Outcome, time.Duration) {})
	resp, err := client.Get(redirector.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("registry 302 到环回地址必须被 SSRF 逐跳复验拒绝")
	}
	if !errors.Is(err, security.ErrEndpointURL) {
		t.Fatalf("错误链应可判 security.ErrEndpointURL（结构性判据）: %v", err)
	}
	if got := innerHits.Load(); got != 0 {
		t.Fatalf("被拒后第二跳不得出站: hits=%d，期望 0", got)
	}
}
