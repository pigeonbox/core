// Package outbound 出站 HTTP 客户端统一注册表（core#87 步骤 1）。
//
// 背景（core#87）：外部 HTTP 调用点三套策略各自演进——notify webhook 推送、
// oidc IdP、federation registry——超时常量散在各自包里（改一处忘两处），且出站
// 调用零观测（外部依赖的延迟与失败率进不了 /metrics，排障只能翻日志）。本包是
// 所有出站调用点的统一取用口：
//
//   - 统一超时基准：DefaultTimeout 兜底 + per-target 显式覆盖（覆盖值迁移自各
//     调用点原值，wire 行为零变化；全部集中在 policies 一张表里）；
//   - 统一出站观测：每次出站打 outbound_requests_total{target,outcome} +
//     outbound_request_duration_seconds{target}（pkg/metrics），失败路径补一条
//     结构化日志（pkg/logger）；
//   - 同 target 复用同一实例 = 连接池复用。
//
// 熔断与重试**有意不做**（issue 步骤 1）：fire-and-forget 推送加重试会把同一条
// 通知发两遍（重复送达）；熔断等本包的 outcome 计数证明某个 target 长期失败后
// 再评估（oekit/httpx 或 concurrency.ErrCircuitOpen）——先量后改，见 issue 步骤 2。
//
// 出站目标必须是固定枚举（指标标签基数有界——固定枚举不是 URL）。
package outbound

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/metrics"
	"github.com/pigeonbox/core/pkg/security"
)

// Target 出站目标枚举（指标标签基数有界——固定枚举不是 URL）。
type Target string

const (
	// TargetNotify 外部通知推送（webhook / telegram / bark；fire-and-forget）。
	TargetNotify Target = "notify"
	// TargetOIDC OIDC IdP（discovery / token / userinfo）。
	TargetOIDC Target = "oidc"
	// TargetFederation 联邦注册中心（p2p registry：注册 / 心跳 / 公告）。
	TargetFederation Target = "federation"
	// TargetFederationDelivery 跨站投递（柜间通道；转发为流式上传）。
	TargetFederationDelivery Target = "federation_delivery"
	// TargetFederationInvite 联邦邀请推送（POST <对端站点>/api/v1/p2p/invite；
	// app/federation pushInvite）。core#116：此前用 http.DefaultClient（零超时 +
	// 盲跟重定向绕过 SSRF 逐跳复验）。
	TargetFederationInvite Target = "federation_invite"
	// TargetFederationDirectory 联邦名录查询（GET <registry>/v1/directory；
	// app/federation PeerDirectory）。core#116：此前同用 http.DefaultClient。
	TargetFederationDirectory Target = "federation_directory"
)

// DefaultTimeout 统一超时基准（core#87：此前三处各写一个常量，改一处忘两处）。
// per-target 覆盖值见 policies——那些是**迁移**不是新决策，改动即 wire 行为变更。
const DefaultTimeout = 15 * time.Second

// Outcome 出站请求结果分类（outbound_requests_total 的 outcome 标签）。
type Outcome string

const (
	// OutcomeOK 底层往返拿到 2xx 响应。
	OutcomeOK Outcome = "ok"
	// OutcomeTransportErr 传输层失败：连接失败 / 客户端超时 / ctx 取消 /
	// 重定向守卫拒绝（CheckRedirect 报错以 url.Error 形态从这里走）。
	OutcomeTransportErr Outcome = "transport_error"
	// OutcomeStatusErr 对端有响应但非 2xx（含重定向跳中的 3xx 那一跳）。
	OutcomeStatusErr Outcome = "status_error"
)

// observer 单次出站观测出口（注入点：测试换桩；默认实现写 metrics、失败补日志）。
type observer func(target Target, outcome Outcome, d time.Duration)

// policy per-target 出站策略。注册表是唯一的客户端构造点：超时 / 重定向守卫等
// 出站策略集中在这张表（原各调用点构造现场的策略随收编迁入，语义逐字保留）。
type policy struct {
	// timeout 客户端级总超时（覆盖连接 / 跳转 / 读响应体全程）。0 = 不设客户端级
	// 超时，由调用方 ctx 截止治理——仅限两个调用点全部自带 deadline 的目标。
	timeout time.Duration
	// checkRedirect 重定向策略；nil = Go 默认（最多跟随 10 跳）。
	checkRedirect func(req *http.Request, via []*http.Request) error
}

// policies 出站策略表——出站策略唯一真源。
//
// ⚠️ 各 target 的覆盖值是**迁移**不是新决策：原值在原调用点的注释里各有出处
// （收编时已随注释迁入对应条目）。改动任一值 = wire 行为变更，须单独拍板。
var policies = map[Target]policy{
	// 5s = 原 app/notify 包内常量（webhook 推送历史口径）。
	// SSRF 重定向守卫（2026-10-05 审计 P3）：webhook URL 仅管理员可配，但保存期
	// 校验会被 302 绕过——目标服务器一跳转即可指向私网。CheckRedirect 逐跳复用
	// security.ValidateEndpointURL（respect security.ssrf.allow_private_networks），
	// 私网目标且未放行时中止。
	TargetNotify: {
		timeout: 5 * time.Second,
		checkRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after 5 redirects")
			}
			if err := security.ValidateEndpointURL(req.URL.String()); err != nil {
				return fmt.Errorf("webhook 重定向目标被拒绝: %w", err)
			}
			return nil
		},
	},
	// 15s = 原 app/oidc 构造现场值，即统一基准本尊。
	TargetOIDC: {timeout: DefaultTimeout},
	// 5s = 原 app/federation/client.go 构造现场值（注册 / 心跳 / 公告短平快调用）。
	TargetFederation: {timeout: 5 * time.Second},
	// **不设客户端级超时**（原 DeliveryClient 语义）：跨站转发是流式上传——
	// client.Timeout 覆盖整个请求周期**含请求体**，设 15s 会腰斩大文件转发；
	// 时长由调用方 ctx 截止治理（两处调用点均带 deadline）。禁跟随重定向：
	// 对端回 302 到内网地址即可借本站出站面绕过 SSRF 判定 ⇒ 3xx 一律按原响应
	// 返回（ErrUseLastResponse，调用方按非 200 判失败）。
	TargetFederationDelivery: {
		checkRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	},
	// core#116 两个联邦出站（原 http.DefaultClient 两处）。10s / 5s 均为**迁移**
	// 不是新决策：原 pushInvite / directory.go 构造现场的 ctx 预算值（两处调用点
	// 仍各自带同值 ctx deadline）。重定向逐跳复验 SSRF（ssrfRedirectGuard，见
	// ssrf_redirect.go 的口径说明）。
	TargetFederationInvite: {
		timeout:       10 * time.Second,
		checkRedirect: ssrfRedirectGuard,
	},
	TargetFederationDirectory: {
		timeout:       5 * time.Second,
		checkRedirect: ssrfRedirectGuard,
	},
}

// registry per-target 共享实例缓存（同 target 复用同一实例 = 连接池复用）。
var (
	regMu    sync.Mutex
	registry = make(map[Target]*http.Client)
	// defaultObserver 生产观测出口（包级变量仅为测试换桩留口；运行期不改写）。
	defaultObserver observer = observeToMetrics
)

// New 返回命名 target 的共享 *http.Client（同 target 复用同一实例=连接池复用）。
//
// 未知 target panic：调用点全是编译期常量（Target* 常量），写错=编程 bug——
// fail-fast 在启动期的构造现场，而不是把错误推迟到第一次出站请求（参照
// regexp.MustCompile 语义；静默回落默认客户端会把策略错配藏进生产流量）。
func New(t Target) *http.Client {
	if _, ok := policies[t]; !ok {
		panic(fmt.Sprintf("outbound: 未注册的出站目标 %q（合法值=Target* 常量，见 policies）", string(t)))
	}
	regMu.Lock()
	defer regMu.Unlock()
	if c, ok := registry[t]; ok {
		return c
	}
	c := buildClient(t, policies[t], defaultObserver)
	registry[t] = c
	return c
}

// buildClient 构造一个挂观测的客户端（注册表内部构造路径；测试用
// newObservedClient 注入自定义观测，不经缓存）。
func buildClient(t Target, p policy, obs observer) *http.Client {
	// clone 默认 Transport：注册表持有独立副本，不影响全局 DefaultTransport
	//（与原 DeliveryClient 构造现场同款做法）。
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{
		Timeout:       p.timeout,
		Transport:     &observingTransport{base: transport, target: t, obs: obs},
		CheckRedirect: p.checkRedirect,
	}
}

// newObservedClient 测试注入点：构造带自定义观测出口的独立客户端（不走缓存，
// 不影响 New 的共享实例）。
func newObservedClient(t Target, obs observer) *http.Client {
	if _, ok := policies[t]; !ok {
		panic(fmt.Sprintf("outbound: 未注册的出站目标 %q", string(t)))
	}
	return buildClient(t, policies[t], obs)
}

// observingTransport 观测 RoundTripper：对每次底层 HTTP 往返记一次
// outcome + 耗时。纯旁路——不改写请求 / 响应 / header，wire 零变化。
// （观测粒度 = 单次 HTTP 往返：响应头到达即计；响应体读取阶段的失败不在此处。）
type observingTransport struct {
	base   http.RoundTripper
	target Target
	obs    observer
}

func (t *observingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	d := time.Since(start)
	switch {
	case err != nil:
		t.obs(t.target, OutcomeTransportErr, d)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		t.obs(t.target, OutcomeStatusErr, d)
	default:
		t.obs(t.target, OutcomeOK, d)
	}
	return resp, err
}

// observeToMetrics 默认观测出口：指标全量记录；失败路径补结构化日志（成功只进
// 指标——fire-and-forget 推送高频，逐条刷日志会把真失败淹没）。
func observeToMetrics(t Target, o Outcome, d time.Duration) {
	metrics.RecordOutboundRequest(string(t), string(o))
	metrics.RecordOutboundDuration(string(t), d.Seconds())
	if o != OutcomeOK {
		logger.Warn("[outbound] 出站请求异常",
			zap.String("target", string(t)),
			zap.String("outcome", string(o)),
			zap.Int64("duration_ms", d.Milliseconds()),
		)
	}
}
