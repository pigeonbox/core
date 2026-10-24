// 下载交付点的「次数耗尽」闸门（2026-10-24）
//
// 语义（设计稿不变量 3）：**次数耗尽 ≠ 过期**；已扣次的那一次取件仍可完成交付。
//
// 下载面的闸门点与其他面**不同**——它在**内容交付那一刻**（chargePickup），
// 而不在授权阶段。这不是实现随意，而是两条不变量的交点：
//   - 若在**授权阶段**就按「次数耗尽」拦 ⇒ 那个**已经付过费**的取件会话
//     （同一下载令牌）会被自己的扣次结果挡住 ⇒ 「已扣次的那一次仍可交付」破。
//   - 交付点扣次天然满足该不变量：同一令牌指纹重复扣次时命中
//     `pickup_charges` 唯一索引 ⇒ 不重复扣、也不报耗尽 ⇒ 继续交付；
//     **新**令牌 = 新的一次取件 ⇒ 额度已用完 ⇒ 报 20004。
//
// 断言必须能变红：
//
//	① 删掉交付点的耗尽判定（ChargePickupOnce 不再报 exhausted）⇒ 新会话用例红；
//	② 把闸门挪到授权阶段（authorize 阶段就拒）⇒ 已扣次会话用例红。
package share

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/pkg/security"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
)

// TestDownload_CountExhausted_BlocksNewSession_AllowsChargedSession
//
// count=1 的文件分享：
//   - 第一次取件（令牌 A）⇒ 交付成功，额度用完；
//   - 同一令牌 A 再下载 ⇒ **仍可交付**（同一次取件，不重复扣次）；
//   - 换新令牌 B（= 新的一次取件）⇒ 拒绝 403 + 20004「超过取件次数」。
func TestDownload_CountExhausted_BlocksNewSession_AllowsChargedSession(t *testing.T) {
	g := newChargeTestEnv(t, true) // 令牌开启（生产默认）
	ctx := t.Context()
	repo := dao.NewFileCodeRepository(g)
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "DLGATE1", Text: "a.bin", FilePath: "uploads", UUIDFileName: "a.bin",
		Size: 10, ExpiredCount: 1, // 次数=1
	}))

	// 取件凭证（令牌 A）
	resp := selectShare(t, "DLGATE1")
	require.NotEmpty(t, resp.Data.Token)
	tokA := "&token=" + url.QueryEscape(resp.Data.Token)

	// ① 第一次取件 ⇒ 交付成功，额度用完
	code, body := performDownloadPath(t, "code=DLGATE1"+tokA)
	require.Equal(t, 200, code, "首次取件应交付成功：%s", body)
	remaining, used := usedCount(t, g, "DLGATE1")
	assert.Equal(t, 0, remaining, "交付时扣 1 次")
	assert.Equal(t, 1, used)

	// ② 同一取件会话（同一令牌）再下载 ⇒ 仍可交付（不重复扣次、也不被自己的
	// 扣次结果挡住）——这是「已扣次的那一次取件仍可完成交付」的核心不变量
	code, body = performDownloadPath(t, "code=DLGATE1"+tokA)
	assert.Equal(t, 200, code, "已扣次的取件会话必须仍能完成交付：%s", body)
	_, used = usedCount(t, g, "DLGATE1")
	assert.Equal(t, 1, used, "同一取件会话不得重复扣次")

	// ③ 新令牌 = 新的一次取件 ⇒ 额度已用完 ⇒ 拒绝（20004，不是 404「不存在/已过期」）
	//
	// 注：新令牌**不能**从 select 取——select 面本身也守次数闸（额度用完后
	// select 直接 20004，这正是「已取过的人仍可再打开」只对**文本交付面**
	// 成立、文件交付面按取件会话计的口径）。这里直接签发令牌模拟「新的一次
	// 取件」：下载面拿令牌扣次时才会发现额度已用完。
	tokB := "&token=" + url.QueryEscape(security.GenerateDownloadToken("DLGATE1"))
	require.NotEmpty(t, tokB)
	code, body = performDownloadPath(t, "code=DLGATE1"+tokB)
	assert.Equal(t, 403, code, "额度用完后新的一次取件应被拒：%s", body)
	assert.Contains(t, body, "20004", "业务码应是 20004「超过取件次数」：%s", body)
}
