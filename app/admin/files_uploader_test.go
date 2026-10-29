// 管理台「上传者」列必须给出**用户名**，而不是裸 user_id（2026-10-29）。
//
// 缺陷：/admin/files/filter 的 item 此前只有 user_id ⇒ 界面只能渲染 "ID: 1"，
// 管理员看不出是谁传的。而**同一仓**的传输日志（/admin/logs/transfer）早已带
// username ⇒ 两条路径口径不一致。
//
// 判据（四格）：
//
//	①活账号 ⇒ 补出用户名；
//	②匿名分享（user_id 为空）⇒ 无用户名（前端回退「匿名」）；
//	③账号已删（分享主人注销）⇒ 用户名留空（前端回退显示 ID）；
//	④多行同属一个用户 ⇒ 一次查询补齐（映射去重，不 N+1）。
//
// 负对照：把 app/admin.fillUploaderNames 的调用去掉，本用例即失败。
package admin

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

func newUploaderTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.User{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1) // :memory: 每连接独立库，钉单连接
	return g
}

// userIDByName 取用户 id（测试夹具）。
func userIDByName(t *testing.T, g *gorm.DB, name string) uint {
	t.Helper()
	var id uint
	require.NoError(t, g.Model(&model.User{}).Where("username = ?", name).Select("id").Scan(&id).Error)
	require.NotZero(t, id, "夹具用户 %s 必须存在", name)
	return id
}

func TestGetFilesFiltered_FillsUploaderName(t *testing.T) {
	g := newUploaderTestDB(t)
	svc := NewService(g)
	ctx := context.Background()

	// 两个活账号 + 一个随后被注销的账号
	//（email 有 UNIQUE 约束 ⇒ 夹具必须各给不同邮箱，否则第二个就撞索引）
	require.NoError(t, g.Create(&model.User{Username: "alice", Email: "alice@example.com", Role: "user", Status: "active"}).Error)
	require.NoError(t, g.Create(&model.User{Username: "bob", Email: "bob@example.com", Role: "user", Status: "active"}).Error)
	require.NoError(t, g.Create(&model.User{Username: "gone", Email: "gone@example.com", Role: "user", Status: "active"}).Error)
	goneID := userIDByName(t, g, "gone")
	require.NoError(t, g.Delete(&model.User{}, goneID).Error) // 注销主人

	aliceID := userIDByName(t, g, "alice")
	bobID := userIDByName(t, g, "bob")

	// 四条分享：alice×2（验证去重）、bob、已注销主人、匿名
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "UPL01", UserID: &aliceID, ExpiredCount: -1}))
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "UPL02", UserID: &aliceID, ExpiredCount: -1}))
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "UPL03", UserID: &bobID, ExpiredCount: -1}))
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "UPL04", UserID: &goneID, ExpiredCount: -1}))
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "UPL05", ExpiredCount: -1})) // 匿名

	files, total, err := svc.GetFilesFiltered(ctx, model.FileCodeQuery{Page: 1, PageSize: 20})
	require.NoError(t, err)
	require.EqualValues(t, 5, total)
	require.Len(t, files, 5)

	byCode := make(map[string]*model.FileCode, len(files))
	for _, f := range files {
		byCode[f.Code] = f
	}

	// ①活账号 ⇒ 用户名
	assert.Equal(t, "alice", byCode["UPL01"].Username, "活账号必须补出用户名")
	assert.Equal(t, "alice", byCode["UPL02"].Username, "同一用户的第二行同样补出（映射复用）")
	assert.Equal(t, "bob", byCode["UPL03"].Username)

	// ③账号已注销 ⇒ 留空（前端回退显示 ID）
	assert.Empty(t, byCode["UPL04"].Username, "主人已注销 ⇒ 用户名留空，不报错不中断")

	// ②匿名 ⇒ 无用户名、无 user_id
	assert.Empty(t, byCode["UPL05"].Username, "匿名分享没有上传者用户名")
	assert.Nil(t, byCode["UPL05"].UserID)

	// user_id 本身必须原样保留（前端回退与筛选都要用）
	assert.NotNil(t, byCode["UPL01"].UserID)
	assert.EqualValues(t, aliceID, *byCode["UPL01"].UserID)
}

// TestGetFilesFiltered_NoUsers_StillReturnsRows 用户表空时列表照常返回
// （补用户名失败不得阻断列表 —— 查询前提是"有数据"，不是"有用户名"）。
func TestGetFilesFiltered_NoUsers_StillReturnsRows(t *testing.T) {
	g := newUploaderTestDB(t)
	svc := NewService(g)
	ctx := context.Background()

	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "NOUSR", ExpiredCount: -1}))

	files, total, err := svc.GetFilesFiltered(ctx, model.FileCodeQuery{Page: 1, PageSize: 20})
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	require.Len(t, files, 1)
	assert.Empty(t, files[0].Username)
}
