package dao

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newGovernanceTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{}))
	// glebarez/sqlite 的 :memory: 每条连接是独立库，多连接会拿到无表空库；
	// 钉死单连接消除该 flake（异步 goroutine 与主流程并发取连接时必现）。
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

func TestUpdateStatusByIDs(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	fc1 := &model.FileCode{Code: "AAA11111"}
	fc2 := &model.FileCode{Code: "BBB22222"}
	require.NoError(t, repo.Create(ctx, fc1))
	require.NoError(t, repo.Create(ctx, fc2))

	t.Run("非法状态拒绝", func(t *testing.T) {
		_, err := repo.UpdateStatusByIDs(ctx, []uint{fc1.ID}, "hacked")
		assert.Error(t, err)
	})
	t.Run("批量禁用后可恢复", func(t *testing.T) {
		n, err := repo.UpdateStatusByIDs(ctx, []uint{fc1.ID, fc2.ID}, model.StatusBlocked)
		require.NoError(t, err)
		assert.Equal(t, int64(2), n)

		got, err := repo.GetByCode(ctx, "AAA11111")
		require.NoError(t, err)
		assert.Equal(t, model.StatusBlocked, got.Status)

		n, err = repo.UpdateStatusByIDs(ctx, []uint{fc1.ID}, model.StatusNormal)
		require.NoError(t, err)
		assert.Equal(t, int64(1), n)
	})
	t.Run("新记录默认normal", func(t *testing.T) {
		require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "CCC33333"}))
		got, err := repo.GetByCode(ctx, "CCC33333")
		require.NoError(t, err)
		// AutoMigrate 默认值：Create 未显式赋 Status 时落库为 normal
		assert.Equal(t, model.StatusNormal, got.Status)
	})
}

func TestListWithFilter(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	uid := uint(7)
	big := int64(1024 * 1024)
	now := time.Now()
	fixtures := []*model.FileCode{
		{Code: "FILEAAAA", UserID: &uid, UploadType: "authenticated", OwnerIP: "1.2.3.4", Size: 100, Status: model.StatusNormal},
		{Code: "FILEBBBB", UploadType: "anonymous", OwnerIP: "5.6.7.8", Size: big, Status: model.StatusBlocked},
		{Code: "FILECCCC", UploadType: "anonymous", OwnerIP: "1.2.3.4", Size: 500, Status: model.StatusNormal},
	}
	for _, f := range fixtures {
		require.NoError(t, repo.Create(ctx, f))
	}
	_ = now

	t.Run("按OwnerIP过滤", func(t *testing.T) {
		files, total, err := repo.ListWithFilter(ctx, model.FileCodeQuery{OwnerIP: "1.2.3.4"})
		require.NoError(t, err)
		assert.Equal(t, int64(2), total)
		assert.Len(t, files, 2)
	})
	t.Run("按状态过滤", func(t *testing.T) {
		files, total, err := repo.ListWithFilter(ctx, model.FileCodeQuery{Status: model.StatusBlocked})
		require.NoError(t, err)
		assert.Equal(t, int64(1), total)
		assert.Equal(t, "FILEBBBB", files[0].Code)
	})
	t.Run("按用户过滤", func(t *testing.T) {
		_, total, err := repo.ListWithFilter(ctx, model.FileCodeQuery{UserID: &uid})
		require.NoError(t, err)
		assert.Equal(t, int64(1), total)
	})
	t.Run("按类型+大小区间", func(t *testing.T) {
		minS, maxS := int64(400), int64(2*1024*1024)
		_, total, err := repo.ListWithFilter(ctx, model.FileCodeQuery{
			UploadType: "anonymous", MinSize: &minS, MaxSize: &maxS,
		})
		require.NoError(t, err)
		assert.Equal(t, int64(2), total) // FILEBBBB(big) + FILECCCC(500)
	})
	t.Run("keyword模糊", func(t *testing.T) {
		_, total, err := repo.ListWithFilter(ctx, model.FileCodeQuery{Keyword: "AAAA"})
		require.NoError(t, err)
		assert.Equal(t, int64(1), total)
	})
	t.Run("组合条件", func(t *testing.T) {
		_, total, err := repo.ListWithFilter(ctx, model.FileCodeQuery{
			OwnerIP: "1.2.3.4", Status: model.StatusNormal,
		})
		require.NoError(t, err)
		assert.Equal(t, int64(2), total)
	})
}

func TestListWithFilter_ExpiredOrEscape(t *testing.T) {
	// 回归：expired=true 的 OR 曾缺外层括号，逃逸 AND 链绕过全部过滤条件
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	uid := uint(7)
	fixtures := []*model.FileCode{
		{Code: "EXPRAAAA", UserID: &uid, OwnerIP: "9.9.9.9", Status: model.StatusNormal, ExpiredCount: 0}, // 次数耗尽
		{Code: "EXPRBBBB", OwnerIP: "8.8.8.8", Status: model.StatusBlocked, ExpiredCount: 0},              // 次数耗尽+blocked
		{Code: "EXPRCCCC", UserID: &uid, OwnerIP: "9.9.9.9", Status: model.StatusNormal, ExpiredCount: 5}, // 正常次数
	}
	for _, f := range fixtures {
		require.NoError(t, repo.Create(ctx, f))
	}

	files, total, err := repo.ListWithFilter(ctx, model.FileCodeQuery{
		UserID:  &uid,
		OwnerIP: "9.9.9.9",
		Status:  model.StatusBlocked,
		Expired: boolPtr(true),
	})
	require.NoError(t, err)
	// 若 OR 逃逸，会返回全站所有 expired_count=0 的记录（含 EXPRBBBB）
	assert.Equal(t, int64(0), total, "组合过滤下无满足项，OR 不得逃逸")
	assert.Empty(t, files)

	// expired=false 与 status 组合同样不被 AND 链破坏
	_, total, err = repo.ListWithFilter(ctx, model.FileCodeQuery{
		OwnerIP: "9.9.9.9",
		Status:  model.StatusNormal,
		Expired: boolPtr(false),
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total) // EXPRCCCC
}

func boolPtr(b bool) *bool { return &b }

func TestFindQuickUploadSource_GovernanceFilter(t *testing.T) {
	// 秒传源检索（2026-10-12 D1=A 同用户内去重）：仅命中同用户 normal 分享；
	// blocked 不得作源（假成功 + 违规内容借尸还魂）；跨用户不得命中
	// （存在性 oracle：撞库探测谁传过什么）；自己的密码分享可作源（无泄露面）。
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	u1, u2 := uint(1), uint(2)
	fixtures := []*model.FileCode{
		{Code: "QOKAAAAA", FilePath: "uploads/a.bin", FileHash: "h1", Size: 100, Status: model.StatusNormal, ExpiredCount: -1, UserID: &u1},
		{Code: "QBLKBBBB", FilePath: "uploads/b.bin", FileHash: "h1", Size: 100, Status: model.StatusBlocked, ExpiredCount: -1, UserID: &u1},
		{Code: "QOTHRCC", FilePath: "uploads/c.bin", FileHash: "h1", Size: 100, Status: model.StatusNormal, ExpiredCount: -1, UserID: &u2},
		{Code: "QPWDDDD", FilePath: "uploads/d.bin", FileHash: "h2", Size: 100, Status: model.StatusNormal, ExpiredCount: -1, RequireAuth: true, PasswordHash: "x", UserID: &u1},
	}
	for _, f := range fixtures {
		require.NoError(t, repo.Create(ctx, f))
	}

	fc, err := repo.FindQuickUploadSource(ctx, u1, "h1", 100)
	require.NoError(t, err)
	assert.Equal(t, "QOKAAAAA", fc.Code, "应命中自己的 normal 分享（跳过 blocked）")

	_, err = repo.FindQuickUploadSource(ctx, u2, "h2", 100)
	assert.Error(t, err, "跨用户不得命中（他人的密码分享更不得）")

	fc, err = repo.FindQuickUploadSource(ctx, u1, "h2", 100)
	require.NoError(t, err, "自己的密码分享可作秒传源（同用户无泄露面）")
	assert.Equal(t, "QPWDDDD", fc.Code)

	_, err = repo.FindQuickUploadSource(ctx, 0, "h1", 100)
	assert.Error(t, err, "匿名（无归属）无秒传")
}

func TestFindQuickUploadSource_SkipsExpiredPicksNewestUsable(t *testing.T) {
	// 回归二（2026-10-10 冒烟 S11b 暴露）：同哈希的过期旧记录不得永久遮蔽
	// 后来的有效分享——此前 First() 按 id 升序取最旧且不过滤过期，一条过期
	// 记录在库即令该文件秒传对所有人永久失效。
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	u1 := uint(1)
	expired := time.Now().Add(-24 * time.Hour)
	fixtures := []*model.FileCode{
		{Code: "QEXPOLD1", FilePath: "uploads/x1.bin", FileHash: "hx", Size: 7, Status: model.StatusNormal, ExpiredCount: -1, ExpiredAt: &expired, UserID: &u1}, // 时间过期
		{Code: "QEXPOLD2", FilePath: "uploads/x2.bin", FileHash: "hx", Size: 7, Status: model.StatusNormal, ExpiredCount: 0, UserID: &u1},                       // 次数耗尽
		{Code: "QFRESH01", FilePath: "uploads/x3.bin", FileHash: "hx", Size: 7, Status: model.StatusNormal, ExpiredCount: -1, UserID: &u1},                      // 有效
	}
	for _, f := range fixtures {
		require.NoError(t, repo.Create(ctx, f))
	}

	fc, err := repo.FindQuickUploadSource(ctx, u1, "hx", 7)
	require.NoError(t, err)
	assert.Equal(t, "QFRESH01", fc.Code, "应跳过过期候选命中最新有效分享")

	// 全部过期 → 未命中（ErrRecordNotFound）
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "QEXPALL1", FilePath: "uploads/y1.bin", FileHash: "hy", Size: 7, Status: model.StatusNormal, ExpiredCount: -1, ExpiredAt: &expired, UserID: &u1,
	}))
	_, err = repo.FindQuickUploadSource(ctx, u1, "hy", 7)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestCountFilePathReferences_SharedObjectGuard(t *testing.T) {
	// 物理删除保护（2026-10-12 秒传共享对象）：引用计数含软删行（回收站行
	// 仍持有物理文件）、含子表行，排除自身分享
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	childRepo := NewFileCodeFileRepository(g)
	ctx := context.Background()

	u1 := uint(1)
	src := &model.FileCode{Code: "REFSRC1", FilePath: "uploads/shared.bin", FileHash: "hh", Size: 9, Status: model.StatusNormal, ExpiredCount: -1, UserID: &u1}
	other := &model.FileCode{Code: "REFOTH1", FilePath: "uploads/shared.bin", FileHash: "hh", Size: 9, Status: model.StatusNormal, ExpiredCount: -1, UserID: &u1}
	require.NoError(t, repo.Create(ctx, src))
	require.NoError(t, repo.Create(ctx, other))

	n, err := repo.CountFilePathReferences(ctx, "uploads/shared.bin", src.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "排除自身后剩 other 一个引用")

	// other 软删（进回收站）后仍计引用——其物理文件留存供恢复
	require.NoError(t, repo.Delete(ctx, other.ID))
	n, err = repo.CountFilePathReferences(ctx, "uploads/shared.bin", src.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "软删行仍持有物理文件，必须计入引用")

	// 子表引用（多文件分享的子文件）
	require.NoError(t, childRepo.CreateBatch(ctx, []*model.FileCodeFile{
		{FileCodeID: src.ID, FileName: "f.bin", FilePath: "uploads/child.bin", Size: 3},
	}))
	n, err = repo.CountFilePathReferences(ctx, "uploads/child.bin", 0)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "子表行计入引用")

	// 全部引用失效（硬删 other + 排除 src 自身）→ 0
	_, err = repo.HardDeleteByIDs(ctx, []uint{other.ID})
	require.NoError(t, err)
	n, err = repo.CountFilePathReferences(ctx, "uploads/shared.bin", src.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}
