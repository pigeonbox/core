package dao

// core#112 「幽灵占用」守卫：预检判据必须与 uniqueIndex 同域。
//
// 唯一索引不区分软删 ⇒ 软删行在 purge 前仍占码。修复前预检不带 Unscoped
// （只看存活行）⇒ 预检放行、Create 撞索引 ⇒ 提示「已被占用」但回收站里
// 找不到占用者。本用例钉住三态判据与 pickup_code 同族面。
//
// 负对照：checkCodeOccupancy / CheckPickupCodeExists 去掉 Unscoped ⇒ 转红。

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckCodeOccupancy_SoftDeletedRowOccupies(t *testing.T) {
	repo := newFoldTestDB(t)
	ctx := context.Background()

	alive := &model.FileCode{Code: "Ghost001"}
	require.NoError(t, repo.Create(ctx, alive))

	t.Run("存活行=OccupiedAlive", func(t *testing.T) {
		occ, err := repo.CheckCodeOccupancy(ctx, "Ghost001", 0)
		require.NoError(t, err)
		assert.Equal(t, CodeOccupiedAlive, occ)
	})

	// 软删进回收站：码仍被占（唯一索引域）
	require.NoError(t, repo.Delete(ctx, alive.ID))

	t.Run("软删行=OccupiedRecycleBin（索引同域）", func(t *testing.T) {
		occ, err := repo.CheckCodeOccupancy(ctx, "Ghost001", 0)
		require.NoError(t, err)
		assert.Equal(t, CodeOccupiedRecycleBin, occ)
		exists, err := repo.CheckCodeExists(ctx, "Ghost001", 0)
		require.NoError(t, err)
		assert.True(t, exists, "预检必须含软删行，否则就是幽灵占用")
	})

	t.Run("折叠形态同域", func(t *testing.T) {
		occ, err := repo.CheckCodeOccupancyFolded(ctx, "ghost001", 0)
		require.NoError(t, err)
		assert.Equal(t, CodeOccupiedRecycleBin, occ)
	})

	t.Run("excludeID 排除自身（含软删自身）", func(t *testing.T) {
		occ, err := repo.CheckCodeOccupancy(ctx, "Ghost001", alive.ID)
		require.NoError(t, err)
		assert.Equal(t, CodeFree, occ)
	})

	t.Run("purge 后码释放", func(t *testing.T) {
		_, err := repo.HardDeleteByIDs(ctx, []uint{alive.ID})
		require.NoError(t, err)
		occ, err := repo.CheckCodeOccupancy(ctx, "Ghost001", 0)
		require.NoError(t, err)
		assert.Equal(t, CodeFree, occ)
	})
}

func TestCheckPickupCodeExists_SoftDeletedRowOccupies(t *testing.T) {
	repo := newFoldTestDB(t)
	ctx := context.Background()
	pc := "PICK001"

	fc := &model.FileCode{Code: "PkGhost1", PickupCode: &pc}
	require.NoError(t, repo.Create(ctx, fc))
	target := &model.FileCode{Code: "PkGhost2"}
	require.NoError(t, repo.Create(ctx, target))
	// 软删占码方（pickup_code 同为 uniqueIndex ⇒ purge 前仍占位）
	require.NoError(t, repo.Delete(ctx, fc.ID))

	taken, err := repo.CheckPickupCodeExists(ctx, pc)
	require.NoError(t, err)
	assert.True(t, taken, "软删行在 purge 前仍占 pickup_code（uniqueIndex 不区分软删）")

	set, err := repo.SetPickupCode(ctx, "PkGhost2", pc)
	require.NoError(t, err, "预检拒绝必须干净返回 ok=false，不得漏出唯一冲突裸错")
	assert.False(t, set)

	_, err = repo.HardDeleteByIDs(ctx, []uint{fc.ID})
	require.NoError(t, err)
	set, err = repo.SetPickupCode(ctx, "PkGhost2", pc)
	require.NoError(t, err)
	assert.True(t, set, "purge 后取件码释放")
}
