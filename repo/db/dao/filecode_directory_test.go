package dao

// 目录配额用量口径断言（core#159）。
//
// 守卫点：
//   - 只统计 dir_root 命中的行（其它目录的行不计）；
//   - 只统计**存活**行：已软删（回收站）与已过期（超过 expired_at）都不计；
//   - 永久分享（expired_at 为空）计入。
//
// ⚠️ 口径与站点级物理配额（GetPhysicalObjectUsage，**含回收站软删**）**故意
// 不同**：站点量「盘上还占多少」、目录量「这个目录对外分享出去多少」。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

func TestGetDirectoryUsage(t *testing.T) {
	g := newHardDeleteLedgerTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	// 目录 A：一条存活（100）+ 一条永久（50）
	require.NoError(t, g.Create(&model.FileCode{
		Code: "DU00001", FilePath: "uploads/a/1.bin", Text: "1", Size: 100,
		DirRoot: "/data/a", ExpiredCount: -1,
	}).Error)
	require.NoError(t, g.Create(&model.FileCode{
		Code: "DU00002", FilePath: "uploads/a/2.bin", Text: "2", Size: 50,
		DirRoot: "/data/a", ExpiredCount: -1,
	}).Error)
	// 目录 A：一条已过期（70）⇒ 不计
	require.NoError(t, g.Create(&model.FileCode{
		Code: "DU00003", FilePath: "uploads/a/3.bin", Text: "3", Size: 70,
		DirRoot: "/data/a", ExpiredCount: -1, ExpiredAt: &past,
	}).Error)
	// 目录 A：一条未过期（30）⇒ 计
	require.NoError(t, g.Create(&model.FileCode{
		Code: "DU00004", FilePath: "uploads/a/4.bin", Text: "4", Size: 30,
		DirRoot: "/data/a", ExpiredCount: -1, ExpiredAt: &future,
	}).Error)
	// 目录 A：一条已软删（90）⇒ 不计
	soft := model.FileCode{
		Code: "DU00005", FilePath: "uploads/a/5.bin", Text: "5", Size: 90,
		DirRoot: "/data/a", ExpiredCount: -1,
	}
	require.NoError(t, g.Create(&soft).Error)
	require.NoError(t, g.Delete(&soft).Error)
	// 目录 B：一条存活（999）⇒ 不计入目录 A
	require.NoError(t, g.Create(&model.FileCode{
		Code: "DU00006", FilePath: "uploads/b/1.bin", Text: "1", Size: 999,
		DirRoot: "/data/b", ExpiredCount: -1,
	}).Error)
	// 无归因的行（其它上传通道）⇒ 不计入任何目录
	require.NoError(t, g.Create(&model.FileCode{
		Code: "DU00007", FilePath: "uploads/c/1.bin", Text: "1", Size: 777,
		ExpiredCount: -1,
	}).Error)

	used, err := repo.GetDirectoryUsage(ctx, "/data/a")
	require.NoError(t, err)
	assert.Equal(t, int64(180), used, "目录用量 = 存活(100+50) + 未过期(30)；过期/软删/他目录/无归因都不计")

	usedB, err := repo.GetDirectoryUsage(ctx, "/data/b")
	require.NoError(t, err)
	assert.Equal(t, int64(999), usedB)

	usedNone, err := repo.GetDirectoryUsage(ctx, "/data/none")
	require.NoError(t, err)
	assert.Equal(t, int64(0), usedNone, "无命中目录的用量为 0")
}
