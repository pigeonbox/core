package share

// core#112 自定义取件码「幽灵占用」生命周期钉子。
//
// 最终语义（修法①，与唯一索引同域）：建码 → 软删 ⇒ 码仍被占（回收站占位），
// 文案点名「永久删除后可复用」；purge 后码释放可复用。软删行自身语义零改动
// （恢复 / purge / 引用计数不受影响）。
//
// 负对照：CheckCodeOccupancy 去掉 Unscoped（回退旧预检）⇒ 「回收站」文案断言
// 当场红（落库撞唯一索引只报「已被占用」）。

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateWithCode_RecycleBinCodeGhost(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	build := func(code string) *model.FileCode {
		return &model.FileCode{Code: code, Status: model.StatusNormal}
	}

	// 建码 → 软删（进回收站）
	fc, err := svc.createWithCode(ctx, "Ghost01", build)
	require.NoError(t, err)
	require.NoError(t, svc.fileCodeRepo.DeleteByIDTx(ctx, fc.ID))

	t.Run("回收站占码期间拒绝复用且文案点名回收站", func(t *testing.T) {
		_, err := svc.createWithCode(ctx, "Ghost01", build)
		require.Error(t, err, "软删行 purge 前占码（与唯一索引同域）")
		assert.Contains(t, err.Error(), "回收站")
		assert.Contains(t, err.Error(), "永久删除")
	})

	t.Run("恢复后=存活行占码（普通文案）", func(t *testing.T) {
		_, err := svc.fileCodeRepo.RestoreByIDs(ctx, []uint{fc.ID})
		require.NoError(t, err)
		_, err = svc.createWithCode(ctx, "Ghost01", build)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "已被占用")
		assert.NotContains(t, err.Error(), "回收站")
	})

	t.Run("purge 后码释放可复用", func(t *testing.T) {
		require.NoError(t, svc.fileCodeRepo.DeleteByIDTx(ctx, fc.ID))
		_, err := svc.fileCodeRepo.HardDeleteByIDs(ctx, []uint{fc.ID})
		require.NoError(t, err)
		got, err := svc.createWithCode(ctx, "Ghost01", build)
		require.NoError(t, err, "purge 后码必须可复用")
		assert.Equal(t, "Ghost01", got.Code)
	})
}

func TestCreateWithCode_RecycleBinCodeGhost_Folded(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	build := func(code string) *model.FileCode {
		return &model.FileCode{Code: code, Status: model.StatusNormal}
	}
	setCodeFold(t, true)

	fc, err := svc.createWithCode(ctx, "Fold01", build)
	require.NoError(t, err)
	require.NoError(t, svc.fileCodeRepo.DeleteByIDTx(ctx, fc.ID))

	_, err = svc.createWithCode(ctx, "fold01", build)
	require.Error(t, err, "折叠形态同样与索引/解析域同域")
	assert.Contains(t, err.Error(), "仅大小写不同")
	assert.Contains(t, err.Error(), "回收站")
}
