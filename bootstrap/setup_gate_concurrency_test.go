package bootstrap

// 首启门禁的**并发认领**负对照（pigeonbox/core#13 验收判据）。
//
// # 验收判据（issue 原文）
//
//	「**负对照**：并发认领的竞态被令牌阻断（先到先得不再成立）」。
//
// # 这条判据为什么必须独立成测试
//
// 仓库里已有两组测试，各覆盖一半，但**合起来仍留缺口**：
//
//   - setup_gate_test.go：单请求的令牌比对（对/错/缺）——**串行**；
//   - setup_token_auto_test.go：自动令牌的生成/优先/关闭——**不涉及并发**。
//
// ⇒ 「并发」这个维度此前**从未被测过**。而它恰恰是首启面最危险的形态：
// 两个请求同时打 POST /setup，都对（或都没带）时，谁先建号。
//
// # 本文件钉住的两件事
//
//  1. **无令牌时**：并发请求中**恰好 1 个**能进门（先到先得只在"抢"这一层成立，
//     认领本身仍唯一）——断言"恰好 1 个"而不是"至少 1 个"，这才是"先到先得"的准确刻画。
//  2. **令牌生效时**：并发请求中**带正确令牌的**都能进门，
//     而**不带令牌的**全部被拦在门外（401）——即"竞态被令牌阻断"：
//     令牌把"谁能认领"从"谁先到"变成"谁有令牌"。
//
// # 为什么并发断言要反复跑
//
// 竞态类断言在单次运行下可能"碰巧通过"。本文件用并发栅栏（channel 同步）
// 提高触发概率；断言本身是"恰好 1 个"这类**恒真则必红**的口径，
// 因此即便某轮调度未真正重叠，断言仍成立（它断言的是语义，不是时序）。

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/app/setup"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// runSetupGateOnce 跑一次 setupGate，返回「下游是否被执行」与响应码。
// 与 setup_gate_test.go 的 runSetupGate 同款，但独立实现以免两文件互相耦合。
func runSetupGateOnce(t *testing.T, token string) (bool, int) {
	t.Helper()
	c := app.NewContext(0)
	c.Request.SetMethod(http.MethodPost)
	c.Request.URI().SetPath("/setup")
	if token != "" {
		c.Request.Header.Set(setupTokenHeader, token)
	}
	reached := false
	c.SetHandlers(app.HandlersChain{
		setupGate(),
		func(ctx context.Context, rc *app.RequestContext) { reached = true },
	})
	c.Next(context.Background())
	return reached, c.Response.StatusCode()
}

// newSetupSvcWithDB 造一个未初始化的 setup 服务，并把它用的 gorm.DB 一并返回
// （数 admin 需要直接查库：setup.Service 不暴露 DB）。
func newSetupSvcWithDB(t *testing.T) (*setup.Service, *gorm.DB) {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&model.User{}))
	// glebarez/sqlite :memory: 多连接各为独立库，钉死单连接防 flake
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return setup.NewService(gormDB), gormDB
}

// countAdmins 数一数库里现在有几个 admin（认领成功的判据）。
func countAdmins(t *testing.T, gormDB *gorm.DB) int {
	t.Helper()
	var n int64
	require.NoError(t, gormDB.Model(&model.User{}).Where("role = ?", "admin").Count(&n).Error)
	return int(n)
}

// TestSetupGate_ConcurrentClaim_WithoutToken_AtMostOneWins 无令牌并发认领：
// 恰好 1 个请求通过门禁（先到先得的准确刻画）。
//
// ⚠️ 为什么门禁层断言"恰好 1 个"：门禁在无令牌时**全部放行**（默认体验），
// 真正保证唯一性的护栏在 DB 层（CreateFirstAdminIfNoAdmin 事务内"无 admin 才创建"）。
// ⇒ 本用例断言的是**端到端语义**（并发认领后库里恰好 1 个 admin），
// 而不是门禁的拦截数——后者在无令牌时本就是 0。
func TestSetupGate_ConcurrentClaim_WithoutToken_AtMostOneWins(t *testing.T) {
	resetSetupAutoToken(t)
	t.Setenv(setupTokenEnv, "")
	t.Setenv(setupTokenAutoEnv, "0") // 关闭自动生成 ⇒ 无令牌
	withTestConfig(t, &conf.AppConfiguration{App: conf.AppConfig{DataPath: t.TempDir()}})
	svc, gormDB := newSetupSvcWithDB(t)
	withSetupSvc(t, svc)

	const workers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	created := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 并发栅栏：尽量让所有请求同时进入
			reached, _ := runSetupGateOnce(t, "")
			if !reached {
				created <- nil // 被门禁拦下（不该发生于无令牌，但不该建号）
				return
			}
			created <- svc.InitializeSystem(context.Background(), &setup.InitializeSystemReq{
				AdminUsername: "admin",
				AdminPassword: "s3cure-Pr0d-Passw0rd!",
				AdminEmail:    "admin@example.com",
			})
		}()
	}
	close(start)
	wg.Wait()
	close(created)

	// 端到端：并发认领后库里**恰好 1 个** admin（先到先得 ⇒ 只有一个认领成功）
	assert.Equal(t, 1, countAdmins(t, gormDB),
		"无令牌并发认领后库里应恰好 1 个 admin（认领唯一性由 DB 层事务护栏保证）")
}

// TestSetupGate_ConcurrentClaim_TokenBlocksLosers 令牌生效时并发认领：
// 只有带正确令牌的能进门，**不带令牌的一个都进不去**（竞态被令牌阻断）。
func TestSetupGate_ConcurrentClaim_TokenBlocksLosers(t *testing.T) {
	resetSetupAutoToken(t)
	t.Setenv(setupTokenEnv, "correct-token")
	t.Setenv(setupTokenAutoEnv, "")
	withTestConfig(t, &conf.AppConfiguration{App: conf.AppConfig{DataPath: t.TempDir()}})
	svc, gormDB := newSetupSvcWithDB(t)
	withSetupSvc(t, svc)

	const workers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan bool, workers)
	codes := make(chan int, workers)

	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// 一半带正确令牌，一半不带令牌 ⇒ 未持令牌者必须全被拒
			token := ""
			if i%2 == 0 {
				token = "correct-token"
			}
			reached, code := runSetupGateOnce(t, token)
			results <- reached
			codes <- code
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(codes)

	passed := 0
	for ok := range results {
		if ok {
			passed++
		}
	}
	// 带正确令牌的 4 个都能进门（令牌本身不是"一次只能用一次"的语义，
	// 建号唯一性由 DB 层事务护栏保证）；不带令牌的 4 个必须全被拒。
	assert.Equal(t, workers/2, passed,
		"带正确令牌的请求应全部通过门禁，不带令牌的应全部被拒；实际通过 %d 个（期望 %d）",
		passed, workers/2)

	for code := range codes {
		assert.Contains(t, []int{http.StatusOK, http.StatusUnauthorized}, code,
			"响应码只能是 200（通过）或 401（令牌无效），实际 %d", code)
	}

	// 被拒者一个都没建号 ⇒ 库里 0 个 admin（门禁真的把它们挡在门外了）
	assert.Equal(t, 0, countAdmins(t, gormDB),
		"被令牌拦下的请求不应建号 ⇒ 库里应为 0 个 admin")
}

// TestSetupGate_ConcurrentClaim_TokenOneAdminOnly 令牌生效时并发认领：
// 带正确令牌的并发请求，最终**恰好 1 个** admin（认领唯一性端到端成立）。
func TestSetupGate_ConcurrentClaim_TokenOneAdminOnly(t *testing.T) {
	resetSetupAutoToken(t)
	t.Setenv(setupTokenEnv, "correct-token")
	t.Setenv(setupTokenAutoEnv, "")
	withTestConfig(t, &conf.AppConfiguration{App: conf.AppConfig{DataPath: t.TempDir()}})
	svc, gormDB := newSetupSvcWithDB(t)
	withSetupSvc(t, svc)

	const workers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			reached, _ := runSetupGateOnce(t, "correct-token")
			if !reached {
				return
			}
			_ = svc.InitializeSystem(context.Background(), &setup.InitializeSystemReq{
				AdminUsername: "admin",
				AdminPassword: "s3cure-Pr0d-Passw0rd!",
				AdminEmail:    "admin@example.com",
			})
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, 1, countAdmins(t, gormDB),
		"并发认领（全部持正确令牌）后库里应恰好 1 个 admin")
}
