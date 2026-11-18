// pickup_count_concurrent_test.go —— 取件计次的并发语义（L5 层：下载计次并发）
//
// ## 真相源
//
// PigeonBox/docs/specs/2026-10-24-pickup-count-semantics-design.md §3 不变量 3：
//
//	「次数耗尽 ⇒ 拒绝**发起新的取件**（20004）；
//	  **已扣次的那一次取件仍可完成交付**（D3 的正解）。」
//
// §2 的 D3（事故级）：按取件次数的分享，走取件码通道时**最后一次取件必然 404
// 拿不到文件**（retrieve 扣到 0 ⇒ 判过期 ⇒ 下载被拒）⇒ 分享页通道却能拿到 ⇒
// 两条通道结论相反。
//
// ## 既有覆盖的缺口（逐条核对过）
//
// repo/db/dao/filecode_pickup_charge_concurrent_test.go（L5 既有主力用例）验的是：
//   - 额度**充足**时同令牌并发 ⇒ 恰好 1 个扣次；
//   - **异**令牌在额度边界上 ⇒ 恰好 1 个扣成功、其余 exhausted；
//   - 扣次失败 ⇒ 撤销登记（失败不消耗次数）。
//
// ⇒ 它**没有**覆盖本文件钉的这一条：**额度已被用完之后，已计次的那个取件会话
// 再来请求，仍必须被当作「本次已交付过」放行**（exhausted=false ⇒ 交付面继续
// 交付），而不是被误判成「次数耗尽」而拒掉。顺序版也没覆盖：
// repo/db/dao/filecode_pickup_charge_test.go 的 ExhaustedReportedNotCharged 只
// 验了**新**令牌被判 exhausted，没验**已计次**令牌在耗尽后仍放行。
//
// ## 为什么并发下最容易破这条
//
// 用户狂点 / 客户端重试 / 多文件逐个下载 ⇒ 同一个取件会话的 N 个请求同时打进来。
// 若把「耗尽」判定**前置**到「本次是否已计次」之前 ⇒ 已付过费的会话被判耗尽 ⇒
// 交付面拒发内容 ⇒ 正是 D3 事故在并发形态下的复现。
//
// ## 负对照（必须能变红，实跑过）
//
// 把 ChargePickupOnce 改成「先判额度、后登记」（耗尽判定前置到登记之前）：
//   - TestChargePickup_ConcurrentSameTokenAfterExhaustionStillDeliverable 红：
//     20 个并发的重复请求全部被判 exhausted=true ⇒ 「已扣次的那一次仍可完成
//     交付」破；且新令牌不再走「登记 → 扣不动 → 撤销登记」路径 ⇒ 计次表残留。
//
// ## 为什么用真并发 DB
//
// app/share 既有测试用 :memory: + SetMaxOpenConns(1)（每条连接是独立库 ⇒ 必须
// 钉死单连接 ⇒ DB 访问被完全串行化 ⇒ 碰不到「登记与扣减争用」这条真实语义）。
// 本文件复用同包既有惯例 newStormService（custom_code_storm_test.go）：临时文件
// SQLite + WAL + busy_timeout + 多连接 ⇒ 真并发。
package share

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/pigeonbox/core/pkg/security"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChargePickup_ConcurrentSameTokenAfterExhaustionStillDeliverable
// 额度用完之后，已计次的取件会话并发重复请求 ⇒ 仍可完成交付。
//
// 三组并发请求同时打进来（模拟「用户狂点 / 客户端重试 / 多文件逐个下载」）：
//   - 20 个请求带**已计次**的同一令牌（fp-A）⇒ 必须全部 (charged=false,
//     exhausted=false) ⇒ 交付面继续交付（**不扣次、不拒发**）；
//   - 5 个请求各带**新**令牌（fp-B*）⇒ 必须全部 (charged=false, exhausted=true)
//     ⇒ 拒绝**发起新的取件**（20004 口径，不是 404「不存在/已过期」）。
//
// 断言口径：
//   - 额度用完前的那一次扣次确实发生（charged=true）；
//   - 耗尽后 expired_count 停在 0（不得扣成负数）、used_count 停在 1；
//   - 计次表恰 1 行（被拒的新令牌必须**撤销登记** ⇒ 否则它们后续会被误判成
//     「已计次」而免费放行一次）。
func TestChargePickup_ConcurrentSameTokenAfterExhaustionStillDeliverable(t *testing.T) {
	// 计次指纹依赖全局密钥（未注入 ⇒ 指纹为空 ⇒ 空指纹每次交付都扣次 ⇒ 判据崩）。
	SetTestSecret(t)

	svc, gormDB := newStormService(t)
	ctx := context.Background()

	const code = "PICKCONC"
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code:         code,
		FilePath:     "uploads/pick-conc.bin",
		Text:         "pick-conc.bin",
		ExpiredCount: 1, // 只够一次取件
	}))

	// 第一次取件（付费）：fp-A 扣掉唯一的一次额度。
	fpA := security.DownloadTokenFingerprint("tok-A")
	require.NotEmpty(t, fpA, "计次指纹不得为空（密钥未注入 ⇒ 指纹为空 ⇒ 判据失效）")
	charged, exhausted, err := svc.ChargePickup(ctx, code, fpA, "download", "10.0.0.1")
	require.NoError(t, err)
	require.True(t, charged, "首次取件必须真扣次")
	require.False(t, exhausted)

	// 并发阶段：20 个「已计次的重复请求」+ 5 个「新取件会话」。
	const repeats = 20
	const fresh = 5

	type outcome struct {
		charged   bool
		exhausted bool
		err       error
	}
	repeatOut := make([]outcome, repeats)
	freshOut := make([]outcome, fresh)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < repeats; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			c, e, err := svc.ChargePickup(ctx, code, fpA, "download", fmt.Sprintf("10.0.0.%d", i+2))
			repeatOut[i] = outcome{charged: c, exhausted: e, err: err}
		}(i)
	}
	for i := 0; i < fresh; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			fp := security.DownloadTokenFingerprint(fmt.Sprintf("tok-B%d", i))
			c, e, err := svc.ChargePickup(ctx, code, fp, "download", fmt.Sprintf("10.1.0.%d", i+1))
			freshOut[i] = outcome{charged: c, exhausted: e, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, o := range repeatOut {
		require.NoError(t, o.err, "第 %d 个重复请求：不得报错", i)
		assert.False(t, o.charged, "第 %d 个重复请求：已计次的会话不得重复扣次", i)
		assert.False(t, o.exhausted,
			"第 %d 个重复请求：**已扣次的那一次取件仍可完成交付**（不得被判次数耗尽）", i)
	}
	for i, o := range freshOut {
		require.NoError(t, o.err, "第 %d 个新取件：不得报错", i)
		assert.False(t, o.charged, "第 %d 个新取件：额度已用完 ⇒ 不得扣次", i)
		assert.True(t, o.exhausted,
			"第 %d 个新取件：额度已用完 ⇒ 必须报 exhausted（拒绝发起新的取件）", i)
	}

	fc, err := svc.fileCodeRepo.GetByCode(ctx, code)
	require.NoError(t, err)
	assert.Equal(t, 0, fc.ExpiredCount, "剩余次数归零（不得扣成负数）")
	assert.Equal(t, 1, fc.UsedCount, "只累加 1 次（重复请求不重复计次）")

	var rows int64
	require.NoError(t, gormDB.Model(&model.PickupCharge{}).Where("code = ?", code).Count(&rows).Error)
	assert.Equal(t, int64(1), rows,
		"计次表恰 1 行（被拒的新令牌必须撤销登记 ⇒ 否则它们后续会被误判成已计次而免费放行）")
}
