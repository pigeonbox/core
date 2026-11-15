// core#182 typed sentinel：GetFileByCode 的错误族必须可判据区分
// 「码确证不存在」（ErrShareGone —— 枚举记账唯一正判据）与
// 「过期」（ErrShareExpired —— 不入桶）。
//
// 断言必须能变红：把 GetFileByCode 的 ErrShareGone 包装去掉（改回裸 err 上抛）
// ⇒ ①红；把 validateShareRecord 的过期改回 errors.New 内联（丢了 typed）
// ⇒ ③红。
package share

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

func TestGetFileByCode_GoneSentinel(t *testing.T) {
	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()

	// ① 码不存在 ⇒ ErrShareGone（handler 据此决定是否记枚举失败）
	_, err := svc.GetFileByCode(ctx, "NOPE1234")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrShareGone),
		"不存在的码必须命中 ErrShareGone：%v", err)

	// ② 过期 ⇒ ErrShareExpired，且不是 ErrShareGone
	past := time.Now().Add(-time.Hour)
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "EXPSENT1", Text: "x", FilePath: "uploads", UUIDFileName: "x.bin",
		Size: 1, ExpiredCount: -1, ExpiredAt: &past,
	}).Error)
	_, err = svc.GetFileByCode(ctx, "EXPSENT1")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrShareExpired),
		"过期应命中 typed sentinel ErrShareExpired：%v", err)
	assert.False(t, errors.Is(err, ErrShareGone),
		"过期不得判成码不存在（否则刷新失效页被记枚举失败）")

	// ③ 基建故障 ⇒ 不是 ErrShareGone（不得伪装成「码不存在」入桶）。
	// 关库会毁掉本用例的 DB，故放最后。
	sqlDB, dbErr := gormDB.DB()
	require.NoError(t, dbErr)
	require.NoError(t, sqlDB.Close())
	_, err = svc.GetFileByCode(ctx, "NOPE5678")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrShareGone),
		"基建故障不得命中 ErrShareGone（否则 DB 故障期全网被记枚举失败）：%v", err)
}
