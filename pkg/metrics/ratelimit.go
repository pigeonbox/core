// 限流降级指标（core#83 阶段 2）。
//
// 此前 Redis 故障时限流器静默降级本地桶——「现在到底在 Redis 限还是在本地限」
// 不可观测（redisx #118 的动机：Redis 故障被静默当成正常限流、或攻击流量被算进
// 故障降级，两者都无法察觉）。本文件给 ResilientLimiter 的 WithFallbackMetrics
// 钩子供数：
//   - ratelimit_fallback_total{scope,source}  非 Redis 判定计数
//     （source ∈ local-fallback / fail-open / fail-closed，取上游 Source 字符串）
//
// scope 是固定枚举（global/upload/download/login），基数有界；与 outbound.go
// 同款恒注册。日志侧由 middleware 节流（每 scope 60s 至多一条 Warn），刷屏面
// 交给计数器承担。
package metrics

import "github.com/prometheus/client_golang/prometheus"

var ratelimitFallback = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "ratelimit_fallback_total",
	Help: "Rate limit decisions not served by Redis, by scope and fallback source.",
}, []string{"scope", "source"})

func init() {
	prometheus.MustRegister(ratelimitFallback)
}

// RecordRatelimitFallback 限流降级判定计数（source 取 redisx ratelimit.Source.String()）。
func RecordRatelimitFallback(scope, source string) {
	ratelimitFallback.WithLabelValues(scope, source).Inc()
}

// RatelimitFallbackVec 返回降级计数器的句柄（观测/测试断言用；勿在热路径
// 外另建第二注册面）。
func RatelimitFallbackVec() *prometheus.CounterVec { return ratelimitFallback }
