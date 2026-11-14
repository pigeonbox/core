// /share/metadata 取件枚举防护负对照（core#163）
//
// 缺陷：POST /anonymous/retrieve 有完整枚举防护（入口 PickupLockedRemain 预检 →
// 429；码不存在 RecordPickupFailure 双维度记账 (IP,码) + 按 IP 枚举），而
// GET /share/metadata/:code 此前两者皆无 ⇒ 枚举只剩 QPS 限流桶
// （50/s vs 锁定 10 次/300s，~4500× 吞吐差）。
//
// 断言必须能变红（负对照）：删掉 ShareMetadata 里的 PickupLockedRemain 预检
// （或 RecordPickupFailure 记账），第 11 次请求落回业务 404 ⇒ 本用例红。
package handler

import (
	"fmt"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/middleware"
)

// newMetadataTestEngine 构造 metadata 面测试引擎（core#163 负对照专用）：
//   - share service：复用同包 share_metadata_limit_test.go 的内存 sqlite 构造；
//   - 锁定器复位为 10 次/300s/锁 600s（lockout 是**全局单例**，不复位会让
//     用例间失败计数串扰）；
//   - 可信代理指向 ut 直连对端 0.0.0.0 ⇒ 每请求经 X-Forwarded-For 注入独立
//     客户端 IP（同上：共用 IP 会让用例互相锁死）。
func newMetadataTestEngine(t *testing.T) *server.Hertz {
	t.Helper()

	// 锁定阈值复位（与 pkg/gate/pickup_test.go 的 withLockout 同规格）
	oldCfg := conf.GetGlobalConfig()
	cfg := &conf.AppConfiguration{}
	cfg.Security.Lockout = conf.LockoutConfig{
		Enabled: true, MaxAttempts: 10, WindowSeconds: 300, LockSeconds: 600,
	}
	conf.SetGlobalConfig(cfg)
	middleware.InitDefaultLockout(nil)
	t.Cleanup(func() {
		conf.SetGlobalConfig(oldCfg)
		middleware.InitDefaultLockout(nil)
	})

	// XFF 采信：ut 无真实连接 ⇒ 直连对端恒为 0.0.0.0；信任它即可让
	// X-Forwarded-For 决定客户端 IP（直连部署的安全默认是不信任——测试显式开）。
	require.NoError(t, middleware.SetTrustedProxies([]string{"0.0.0.0/8"}))
	t.Cleanup(func() { _ = middleware.SetTrustedProxies(nil) })

	svc, _ := newMetadataHandlerSvc(t)
	SetShareService(svc)
	t.Cleanup(func() { SetShareService(nil) })

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/share/metadata/:code", ShareMetadata)
	return h
}

// doGet 发一次 GET，客户端来源 IP 经 X-Forwarded-For 注入（见 newMetadataTestEngine）。
func doGet(e *server.Hertz, path, ip string) *ut.ResponseRecorder {
	return ut.PerformRequest(e.Engine, "GET", path, nil,
		ut.Header{Key: "Host", Value: "test.local"},
		ut.Header{Key: "X-Forwarded-For", Value: ip})
}

// TestMetadata_EnumLockoutAfterTenMisses core#163：metadata 入口此前不记账不预检——
// 同一 IP 打 10 个不存在的码，第 11 次必须 429（与 /anonymous/retrieve 同口径）。
func TestMetadata_EnumLockoutAfterTenMisses(t *testing.T) {
	e := newMetadataTestEngine(t)
	ip := "203.0.113.7" // 每用例独立 IP，避开全局 lockout 串扰
	for i := 0; i < 10; i++ {
		w := doGet(e, fmt.Sprintf("/share/metadata/%06d", 900000+i), ip)
		if w.Code != 404 && w.Code != 200 {
			// 前 10 次应是业务响应（不存在的码 = 统一 404），不是 429
			t.Fatalf("第 %d 次应为业务响应, got %d", i+1, w.Code)
		}
	}
	w := doGet(e, "/share/metadata/900009", ip)
	if w.Code != 429 {
		t.Fatalf("第 11 次必须被枚举锁定 429, got %d", w.Code)
	}
}
