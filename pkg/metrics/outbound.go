// 出站 HTTP 指标（core#87 步骤 1）。
//
// 此前出站调用（notify webhook / oidc IdP / federation registry）零观测——外部
// 依赖的延迟与失败率进不了 /metrics，排障只能翻日志。本文件给统一出站注册表
// （pkg/outbound）供数：
//   - outbound_requests_total{target,outcome}    出站请求计数（outcome ∈ ok/transport_error/status_error）
//   - outbound_request_duration_seconds{target}  出站请求延迟直方图
//
// target 是固定枚举（pkg/outbound.Target），基数有界；与本文件业务指标同款
// 恒注册（计数器廉价无需开关），值仅在实例挂载 /metrics 端点时可见。
package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	outboundRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "outbound_requests_total",
		Help: "Outbound HTTP requests by target and outcome.",
	}, []string{"target", "outcome"})

	outboundDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "outbound_request_duration_seconds",
		Help: "Outbound HTTP request duration by target.",
		// 上沿覆盖统一超时基准 15s（DefBuckets 止于 10，超时挂死会全落 +Inf 分桶）。
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15},
	}, []string{"target"})
)

func init() {
	prometheus.MustRegister(outboundRequests, outboundDuration)
}

// RecordOutboundRequest 出站请求计数（target/outcome 取 pkg/outbound 的枚举字符串）。
func RecordOutboundRequest(target, outcome string) {
	outboundRequests.WithLabelValues(target, outcome).Inc()
}

// RecordOutboundDuration 出站请求延迟（秒）。
func RecordOutboundDuration(target string, seconds float64) {
	outboundDuration.WithLabelValues(target).Observe(seconds)
}
