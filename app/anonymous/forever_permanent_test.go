// core#137 服务级判据：CreateAnonymousShare(ExpireAt=nil) 建出的分享**真不过期**。
//
// 修复前：ExpireAt=nil 直接报「ExpireAt 必填」⇒ 永久语义进不来，调用侧
// （gen/handler/share_anonymous GenerateCode）只能塞默认 24h ⇒ 用户选「永久」
// 拿到 24 小时过期的分享。nil=永久与 MintForShare 的既有约定一致：
// DB expired_at NULL = 永久（IsExpired 恒 false、janitor 跳过 NULL）。
//
// 负对照（已实跑）：把 nil 放行改回 `return "", errors.New("ExpireAt 必填")`
// ⇒ 本判据转红；还原后全绿。
package anonymous

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

func TestCreateAnonymousShare_NilExpireAtIsPermanent(t *testing.T) {
	svc, _, g := newTestService(t)
	ctx := context.Background()

	code, err := svc.CreateAnonymousShare(ctx, AnonymousShareParams{
		FilePath: "", FileName: "perm.bin", FileSize: 9,
		ExpireAt: nil, MaxPickupCount: -1,
	})
	require.NoError(t, err, "ExpireAt=nil = 永久分享，必须放行（修前报「ExpireAt 必填」）")
	assert.Len(t, code, codeLength)

	var fc model.FileCode
	require.NoError(t, g.Where("uuid_file_name = ?", "perm.bin").First(&fc).Error)
	assert.Nil(t, fc.ExpiredAt, "永久 = DB expired_at NULL")
	assert.False(t, fc.IsExpired(), "永久分享在任何时刻都不判过期")

	// 端到端可取：Peek 的时间过期闸（IsExpired）不得拦永久分享
	_, peeked, err := svc.Peek(ctx, code)
	require.NoError(t, err, "永久分享必须可 Peek")
	assert.Nil(t, peeked.ExpiredAt)

	// 正判据护栏：非空 ExpireAt 行为不动（防「一律落 NULL」式改坏）
	exp := time.Now().Add(time.Hour)
	_, err = svc.CreateAnonymousShare(ctx, AnonymousShareParams{
		FilePath: "", FileName: "timed.bin", FileSize: 9,
		ExpireAt: &exp, MaxPickupCount: -1,
	})
	require.NoError(t, err)
	var timed model.FileCode
	require.NoError(t, g.Where("uuid_file_name = ?", "timed.bin").First(&timed).Error)
	require.NotNil(t, timed.ExpiredAt, "带过期的分享必须落非空 expired_at")
	assert.InDelta(t, 3600, time.Until(*timed.ExpiredAt).Seconds(), 60)
}
