package share

import (
	"context"
	"errors"
	"testing"

	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 回收站恢复语义守卫：restore 只对"当前处于软删态"的分享生效。
// 回归背景（2026-10-08 全功能验证）：RestoreByCode 不查 RowsAffected，
// 硬删/不存在的 code 恢复返回 200 静默 no-op——与 v0.11.1 修过的
// hard-delete no-op 同构（0 行也成功，运维/集成误判已恢复）。
func TestRestoreUserShare_NotInRecycleBin(t *testing.T) {
	gormDB := newTestDB(t)
	svc := NewService(gormDB, "http://test", nil)
	ctx := context.Background()
	repo := dao.NewFileCodeRepository(gormDB)

	uid := uint(7)
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "RCY0001", UserID: &uid}))

	// 软删 → 硬删 → 恢复必须显式报错（不再是 200 no-op）
	n, err := repo.BatchSoftDeleteByCodes(ctx, 7, []string{"RCY0001"})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.NoError(t, svc.HardDeleteUserShare(ctx, 7, "RCY0001"))

	err = svc.RestoreUserShare(ctx, 7, "RCY0001")
	require.Error(t, err, "硬删后恢复不得静默成功")
	assert.True(t, errors.Is(err, ErrNotInRecycleBin), "应映射为 ErrNotInRecycleBin, got: %v", err)

	// 不存在的 code 同样显式报错
	err = svc.RestoreUserShare(ctx, 7, "NOPE0001")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotInRecycleBin))
}

func TestRestoreUserShare_RestoresSoftDeleted(t *testing.T) {
	gormDB := newTestDB(t)
	svc := NewService(gormDB, "http://test", nil)
	ctx := context.Background()
	repo := dao.NewFileCodeRepository(gormDB)

	uid := uint(7)
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "RCY0002", UserID: &uid}))
	n, err := repo.BatchSoftDeleteByCodes(ctx, 7, []string{"RCY0002"})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	require.NoError(t, svc.RestoreUserShare(ctx, 7, "RCY0002"))

	fc, err := repo.GetByCode(ctx, "RCY0002")
	require.NoError(t, err)
	require.NotNil(t, fc)
	assert.False(t, fc.DeletedAt.Valid, "恢复后 deleted_at 应失效")

	// 已恢复（活跃）的分享再次恢复 → 0 行，显式报错
	err = svc.RestoreUserShare(ctx, 7, "RCY0002")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotInRecycleBin))
}
