// 匿名取件面的「次数耗尽」口径回归（2026-10-24）
//
// 缺陷：model.FileCode.IsExpired() 拆分前**含次数语义**（ExpiredCount==0 ⇒
// 过期）⇒ 匿名通道在额度用完后，Peek 面把它报成「码不存在」。拆分后
// IsExpired() 只判时间 ⇒ 匿名面的三个出口各自漏掉了次数闸：
//
//   - /anonymous/retrieve：已有 ErrCodeExhausted ⇒ 20007 ✅
//   - /anonymous/download：已有 IsCountExhausted ⇒ 20007 ✅
//   - /anonymous/search  ：**漏** ⇒ 额度用完后仍回显文件名/大小/取件统计
//     （旧口径是 404）
//
// 另一个方向的坑：若把次数闸放进 Peek（而不是放在调用方），/anonymous/download
// 就会先拿到 ErrCodeNotFound ⇒ 20007 这条独立错误码永远发不出来 ⇒
// 「次数耗尽」又退回了「不存在/已过期」的伪装。
//
// 断言必须能变红：① 删掉 SearchByCode 里的 IsCountExhausted 闸 ⇒ 本用例失败；
// ② 把 Peek 里的耗尽判定挪进来（吞成 ErrCodeNotFound）⇒ Peek 用例失败。
package anonymous

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// TestPeek_CountExhaustedStillVisible 次数耗尽时，Peek **不得**把它吞成
// 「码不存在」——Peek 只判时间过期，耗尽态交由调用方按自己的口径给
// 20004/20007（吞掉 ⇒ 20007 永远发不出来）。
func TestPeek_CountExhaustedStillVisible(t *testing.T) {
	svc, _, g := newTestService(t)
	ctx := context.Background()

	shareCode := "PEEK001"
	require.NoError(t, g.Create(&model.FileCode{
		Code: shareCode,
		Text: "peek.txt",
		// count 样式分享：只有次数上限，没有时间上限（ExpiredAt=nil ⇒ 永不过期）
		ExpiredCount: 0, // 额度已用完
	}).Error)

	meta, fc, err := svc.Peek(ctx, shareCode)
	require.NoError(t, err, "额度用完不是「不存在」：Peek 不得吞掉耗尽态")
	require.NotNil(t, fc)
	require.Equal(t, shareCode, fc.Code)
	require.NotNil(t, meta)
	assert.True(t, fc.IsCountExhausted(), "记录本身应处于耗尽态")
}

// TestRetrieve_CountExhausted 匿名取件在额度用完后给独立错误码（20007 对应的
// ErrCodeExhausted），不是「码不存在」。
func TestRetrieve_CountExhausted(t *testing.T) {
	svc, _, g := newTestService(t)
	ctx := context.Background()

	shareCode := "RETR001"
	require.NoError(t, g.Create(&model.FileCode{
		Code:         shareCode,
		Text:         "secret",
		ExpiredCount: 0,
	}).Error)

	_, err := svc.Retrieve(ctx, shareCode, "", "1.2.3.4")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCodeExhausted, "额度用完应给 ErrCodeExhausted（→20007）")
	require.NotErrorIs(t, err, ErrCodeNotFound, "不得伪装成「码不存在」")
}
