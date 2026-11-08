package dao

// queriespec_pilot_test.go —— core#88 三处 QuerySpec 试点的判据锁。
//
// 试点三处：ListWithFilter（管理台文件列表）/ GetUserSharesWithFilter（用户
// 分享列表）/ TransferLogRepository.List（传输日志审计面）。判据三条：
//
//  1. EXPLAIN QUERY PLAN 与替换前逐条一致：golden 数据于 2026-11-09 在本文件
//     同款 schema（AutoMigrate）/种子/参数矩阵下从**替换前手拼实现**录制
//     （sqlite=glebarez 纯 Go 驱动，:memory: 单连接，无 ANALYZE ⇒ 计划只由
//     schema 与查询形状决定，已验证跨次运行逐字节确定）。只比对 plan 步骤的
//     detail 列（id/parent/notused 是 sqlite 内部编号，不承载语义）。
//     条件词序是计划一致性的前提（sqlite 按 WHERE 词序做索引分析）⇒ 改动
//     spec 构建函数的条件顺序前先跑本测试。
//  2. fail-closed：空过滤条件不得退化成无约束查询——三处各自的基线约束
//     （用户面 user_id 租户约束 / 两处 gorm 软删 scope）必须在场；行为断言
//     与 SQL 捕获双通道（SQL 通道直接断言「不产生无 WHERE 的 SELECT」）。
//     Deleted="all" 是显式全量语义（admin 明示含已删），单独用例锁定。
//  3. 分页口径未动：normalizePage/normalizeUserSharePage/paginate 本轮零
//     改动，用例以捕获 SQL 验证 LIMIT 归一化结果与手拼时代一致。

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/oekit/gormx/crud"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ==================== SQL 捕获与 EXPLAIN 辅助 ====================

// sqlRec 捕获一条已构建 SQL（占位符形态）与其绑定参数。
type sqlRec struct {
	SQL  string
	Vars []any
}

// newCaptureDB 开 :memory: 库并挂 SQL 捕获回调（查询回调之后取已构建 SQL）。
// glebarez/sqlite 的 :memory: 每条连接是独立库，钉死单连接（与
// newGovernanceTestDB 同款处理）。
func newCaptureDB(t *testing.T) (*gorm.DB, *[]sqlRec) {
	t.Helper()
	rec := make([]sqlRec, 0, 8)
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{}, &model.TransferLog{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	capture := func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.SQL.Len() == 0 {
			return
		}
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(tx.Statement.SQL.String())), "SELECT") {
			return
		}
		rec = append(rec, sqlRec{SQL: tx.Statement.SQL.String(), Vars: append([]any(nil), tx.Statement.Vars...)})
	}
	require.NoError(t, g.Callback().Query().After("gorm:query").Register("capture_sql", capture))
	return g, &rec
}

// explainPlans 对一条 SQL 跑 EXPLAIN QUERY PLAN，返回逐行 detail。
func explainPlans(t *testing.T, g *gorm.DB, r sqlRec) []string {
	t.Helper()
	rows, err := g.Raw("EXPLAIN QUERY PLAN "+r.SQL, r.Vars...).Rows()
	require.NoError(t, err, "explain: %s", r.SQL)
	defer func() { _ = rows.Close() }() // errcheck：Explain 后不消费 Close 错误（行数极少、只读连接）
	var out []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		out = append(out, detail)
	}
	require.NoError(t, rows.Err())
	return out
}

// capturedPlans 汇总捕获到的全部 SELECT 语句的 plan（count 与 find 依次拼接）。
func capturedPlans(t *testing.T, g *gorm.DB, rec *[]sqlRec) []string {
	t.Helper()
	var out []string
	for _, r := range *rec {
		out = append(out, explainPlans(t, g, r)...)
	}
	return out
}

// ==================== 判据 1：EXPLAIN QUERY PLAN 逐条一致 ====================

// goldenQueryPlans 替换前手拼实现的 EXPLAIN QUERY PLAN（detail 列，逐语句）。
// 录制环境与断言口径见文件头注释。key = 「函数/过滤组合」，value 按捕获顺序
// 依次为 Count 语句与 Find 语句的 plan 步骤。
var goldenQueryPlans = map[string][]string{
	"ListWithFilter/空过滤": {
		`SEARCH file_codes USING COVERING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"ListWithFilter/keyword": {
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"ListWithFilter/userID+uploadType": {
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"ListWithFilter/ownerIP+status+size区间": {
		`SEARCH file_codes USING INDEX idx_file_codes_status_expired (status=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_status_expired (status=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"ListWithFilter/created区间": {
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"ListWithFilter/expired_true": {
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"ListWithFilter/expired_false": {
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"ListWithFilter/deleted_only": {
		`SEARCH file_codes USING COVERING INDEX idx_file_codes_deleted_at (deleted_at>?)`,
		`SCAN file_codes`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"ListWithFilter/deleted_all": {
		`SCAN file_codes USING COVERING INDEX idx_file_codes_deleted_at`,
		`SCAN file_codes`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"ListWithFilter/health_active": {
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"ListWithFilter/health_expired": {
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"ListWithFilter/health_expiring_soon": {
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"ListWithFilter/health_never_picked": {
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"ListWithFilter/health_forever": {
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_deleted_at (deleted_at=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"ListWithFilter/组合全条件": {
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"GetUserSharesWithFilter/空过滤": {
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"GetUserSharesWithFilter/active": {
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"GetUserSharesWithFilter/expired": {
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"GetUserSharesWithFilter/text": {
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"GetUserSharesWithFilter/file": {
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"GetUserSharesWithFilter/viewed": {
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"GetUserSharesWithFilter/deleted": {
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"GetUserSharesWithFilter/search": {
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"GetUserSharesWithFilter/search+file": {
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`SEARCH file_codes USING INDEX idx_file_codes_user_id (user_id=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"TransferLog.List/空过滤": {
		`SEARCH transfer_logs USING COVERING INDEX idx_transfer_logs_deleted_at (deleted_at=?)`,
		`SEARCH transfer_logs USING INDEX idx_transfer_logs_deleted_at (deleted_at=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"TransferLog.List/operation": {
		`SEARCH transfer_logs USING INDEX idx_transfer_logs_operation (operation=?)`,
		`SEARCH transfer_logs USING INDEX idx_transfer_logs_operation (operation=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"TransferLog.List/userID": {
		`SEARCH transfer_logs USING INDEX idx_transfer_logs_user_id (user_id=?)`,
		`SEARCH transfer_logs USING INDEX idx_transfer_logs_user_id (user_id=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"TransferLog.List/search": {
		`SEARCH transfer_logs USING INDEX idx_transfer_logs_deleted_at (deleted_at=?)`,
		`SEARCH transfer_logs USING INDEX idx_transfer_logs_deleted_at (deleted_at=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
	"TransferLog.List/组合": {
		`SEARCH transfer_logs USING INDEX idx_transfer_logs_operation (operation=?)`,
		`SEARCH transfer_logs USING INDEX idx_transfer_logs_operation (operation=?)`,
		`USE TEMP B-TREE FOR ORDER BY`,
	},
}

// seedPilotFixtures 种子数据：结构覆盖三处查询的过滤维度（plan 与行数无关，
// 无 ANALYZE；种子只为让查询有行可走并顺带验证可执行）。
func seedPilotFixtures(t *testing.T, g *gorm.DB) (repo *FileCodeRepository, logRepo *TransferLogRepository, uid uint) {
	t.Helper()
	ctx := context.Background()
	repo = NewFileCodeRepository(g)
	logRepo = NewTransferLogRepository(g)
	uid = 7
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(24 * time.Hour)
	fixtures := []*model.FileCode{
		{Code: "FILEAAAA", UserID: &uid, UploadType: "authenticated", OwnerIP: "1.2.3.4", Size: 100, Status: model.StatusNormal, FilePath: "uploads/a.txt", Text: "a.txt", ExpiredCount: -1},
		{Code: "FILEBBBB", UploadType: "anonymous", OwnerIP: "5.6.7.8", Size: 1024 * 1024, Status: model.StatusBlocked, FilePath: "uploads/b.bin", Text: "b.bin", ExpiredCount: 0, ExpiredAt: &past},
		{Code: "FILECCCC", UploadType: "anonymous", OwnerIP: "1.2.3.4", Size: 500, Status: model.StatusNormal, FilePath: "uploads/c.txt", Text: "c.txt", ExpiredCount: 5, ExpiredAt: &future, ViewerAt: &past},
		{Code: "TEXT0001", UserID: &uid, Text: "普通文本", ExpiredCount: -1},
	}
	for _, f := range fixtures {
		require.NoError(t, repo.Create(ctx, f))
	}
	for _, l := range []*model.TransferLog{
		{Operation: "upload", FileCode: "FILEAAAA", FileName: "a.txt", UserID: &uid, Username: "u7", IP: "1.2.3.4"},
		{Operation: "download", FileCode: "FILEBBBB", FileName: "b.bin", Username: "anon", IP: "5.6.7.8"},
	} {
		require.NoError(t, logRepo.Create(ctx, l))
	}
	return repo, logRepo, uid
}

// TestQueriesSpecPilot_ExplainQueryPlanUnchanged 三处试点查询的执行计划与
// 替换前手拼实现逐条一致（判据 1）。矩阵覆盖：全部过滤字段单独、Or 组合
// 分支（expired 双向 / 健康 5 态 / 回收站 3 态）、以及全字段叠加的组合条件。
// 条件词序被本测试钉住：sqlite 按 WHERE 词序做索引分析，重排条件必须
// 显式重录 golden（改本表 + 注明重录原因），不允许静默漂移。
func TestQueriesSpecPilot_ExplainQueryPlanUnchanged(t *testing.T) {
	g, rec := newCaptureDB(t)
	repo, logRepo, uid := seedPilotFixtures(t, g)
	ctx := context.Background()

	minS, maxS := int64(400), int64(2*1024*1024)
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(24 * time.Hour)
	expiredTrue, expiredFalse := true, false

	type adminCase struct {
		name  string
		query model.FileCodeQuery
	}
	type userCase struct {
		name   string
		filter UserShareFilter
	}
	type logCase struct {
		name  string
		query model.TransferLogQuery
	}

	adminCases := []adminCase{
		{"空过滤", model.FileCodeQuery{}},
		{"keyword", model.FileCodeQuery{Keyword: "AAAA"}},
		{"userID+uploadType", model.FileCodeQuery{UserID: &uid, UploadType: "anonymous"}},
		{"ownerIP+status+size区间", model.FileCodeQuery{OwnerIP: "1.2.3.4", Status: model.StatusNormal, MinSize: &minS, MaxSize: &maxS}},
		{"created区间", model.FileCodeQuery{CreatedAfter: &past, CreatedBefore: &future}},
		{"expired_true", model.FileCodeQuery{Expired: &expiredTrue}},
		{"expired_false", model.FileCodeQuery{Expired: &expiredFalse}},
		{"deleted_only", model.FileCodeQuery{Deleted: "only"}},
		{"deleted_all", model.FileCodeQuery{Deleted: "all"}},
		{"health_active", model.FileCodeQuery{Health: "active"}},
		{"health_expired", model.FileCodeQuery{Health: "expired"}},
		{"health_expiring_soon", model.FileCodeQuery{Health: "expiring_soon"}},
		{"health_never_picked", model.FileCodeQuery{Health: "never_picked"}},
		{"health_forever", model.FileCodeQuery{Health: "forever"}},
		{"组合全条件", model.FileCodeQuery{Keyword: "txt", UserID: &uid, UploadType: "authenticated", OwnerIP: "1.2.3.4", Status: model.StatusNormal, MinSize: &minS, MaxSize: &maxS, CreatedAfter: &past, CreatedBefore: &future, Expired: &expiredFalse, Health: "active"}},
	}
	userCases := []userCase{
		{"空过滤", UserShareFilter{}},
		{"active", UserShareFilter{Status: "active"}},
		{"expired", UserShareFilter{Status: "expired"}},
		{"text", UserShareFilter{Status: "text"}},
		{"file", UserShareFilter{Status: "file"}},
		{"viewed", UserShareFilter{Status: "viewed"}},
		{"deleted", UserShareFilter{Status: "deleted"}},
		{"search", UserShareFilter{Status: "all", Search: "AAAA"}},
		{"search+file", UserShareFilter{Status: "file", Search: "txt"}},
	}
	logCases := []logCase{
		{"空过滤", model.TransferLogQuery{}},
		{"operation", model.TransferLogQuery{Operation: "upload"}},
		{"userID", model.TransferLogQuery{UserID: &uid}},
		{"search", model.TransferLogQuery{Search: "AAAA"}},
		{"组合", model.TransferLogQuery{Operation: "download", UserID: &uid, Search: "bin"}},
	}

	produced := map[string]bool{}
	run := func(label string, fn func(ctx context.Context) error) {
		*rec = (*rec)[:0]
		if err := fn(ctx); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		t.Run(label, func(t *testing.T) {
			got := capturedPlans(t, g, rec)
			want, ok := goldenQueryPlans[label]
			require.True(t, ok, "矩阵新增组合 %q 无 golden 记录：先在替换前实现上重录（见文件头）", label)
			assert.Equal(t, want, got, "EXPLAIN QUERY PLAN 与替换前不一致（%s）", label)
			produced[label] = true
		})
	}

	for _, tc := range adminCases {
		tc := tc
		tc.query.Page, tc.query.PageSize = 1, 20
		run("ListWithFilter/"+tc.name, func(ctx context.Context) error {
			_, _, err := repo.ListWithFilter(ctx, tc.query)
			return err
		})
	}
	for _, tc := range userCases {
		tc := tc
		tc.filter.Page, tc.filter.PageSize = 1, 20
		run("GetUserSharesWithFilter/"+tc.name, func(ctx context.Context) error {
			_, _, err := repo.GetUserSharesWithFilter(ctx, uid, tc.filter)
			return err
		})
	}
	for _, tc := range logCases {
		tc := tc
		tc.query.Page, tc.query.PageSize = 1, 20
		run("TransferLog.List/"+tc.name, func(ctx context.Context) error {
			_, _, err := logRepo.List(ctx, tc.query)
			return err
		})
	}

	// golden 不得有矩阵未覆盖的陈旧条目（防「golden 有、查询没了」的假绿）
	var stale []string
	for k := range goldenQueryPlans {
		if !produced[k] {
			stale = append(stale, k)
		}
	}
	assert.Empty(t, stale, "golden 中存在矩阵未覆盖的陈旧条目: %v", stale)
}

// ==================== 判据 2：fail-closed（空条件不得退化成无约束查询） ====================

// TestQueriesSpecPilot_FailClosed_EmptyFilterStaysConstrained 三处试点在
// 「全空筛选」下仍必须携带各自基线约束——用户面 user_id 租户约束 / 管理台
// 与日志面 gorm 软删 scope。双通道断言：行为（越界行不可见）+ SQL 捕获
// （SELECT 必须带 WHERE 约束片段，不产生无 WHERE 查询）。
func TestQueriesSpecPilot_FailClosed_EmptyFilterStaysConstrained(t *testing.T) {
	ctx := context.Background()

	t.Run("GetUserSharesWithFilter_空筛选仍带租户约束", func(t *testing.T) {
		g, rec := newCaptureDB(t)
		repo := NewFileCodeRepository(g)
		mine := &model.FileCode{Code: "FCMINE01", Text: "mine", UserID: uintPtr(1), ExpiredCount: -1}
		others := &model.FileCode{Code: "FCOTHR02", Text: "others", UserID: uintPtr(2), ExpiredCount: -1}
		require.NoError(t, repo.Create(ctx, mine))
		require.NoError(t, repo.Create(ctx, others))

		items, total, err := repo.GetUserSharesWithFilter(ctx, 1, UserShareFilter{Page: 1, PageSize: 20})
		require.NoError(t, err)
		assert.Equal(t, int64(1), total, "空筛选不得越过 user_id 租户约束（其他用户的行不得出现）")
		require.Len(t, items, 1)
		assert.Equal(t, "FCMINE01", items[0].Code)

		require.NotEmpty(t, *rec)
		for _, r := range *rec {
			assert.Contains(t, r.SQL, "user_id = ?", "SELECT 必须携带租户约束: %s", r.SQL)
		}
	})

	t.Run("GetUserSharesWithFilter_spec构建器空筛选仍有条件", func(t *testing.T) {
		// 构建器级 fail-closed：空筛选的 spec 不得是空条件集（user_id 在场）
		spec := userShareQuerySpec(1, UserShareFilter{}).Spec()
		require.NotEmpty(t, spec.Conds, "空筛选的 spec 不得为空条件集")
		assert.Equal(t, "user_id = ?", spec.Conds[0].Query, "租户约束必须是首个条件")
	})

	t.Run("ListWithFilter_空筛选仍带软删约束", func(t *testing.T) {
		g, rec := newCaptureDB(t)
		repo := NewFileCodeRepository(g)
		live := &model.FileCode{Code: "FCLIVE01", Status: model.StatusNormal}
		dead := &model.FileCode{Code: "FCDEAD02", Status: model.StatusNormal}
		require.NoError(t, repo.Create(ctx, live))
		require.NoError(t, repo.Create(ctx, dead))
		require.NoError(t, repo.Delete(ctx, dead.ID)) // 软删进回收站

		items, total, err := repo.ListWithFilter(ctx, model.FileCodeQuery{Page: 1, PageSize: 20})
		require.NoError(t, err)
		assert.Equal(t, int64(1), total, "空筛选默认视图必须带软删约束（回收站行不得出现）")
		require.Len(t, items, 1)
		assert.Equal(t, "FCLIVE01", items[0].Code)

		require.NotEmpty(t, *rec)
		for _, r := range *rec {
			assert.Contains(t, r.SQL, "`file_codes`.`deleted_at` IS NULL", "SELECT 必须携带软删约束: %s", r.SQL)
		}
	})

	t.Run("ListWithFilter_Deleted_all是显式全量语义", func(t *testing.T) {
		// 「空条件 ⇒ 全表」的唯一合法出口：admin 显式传 Deleted="all"（含已删）。
		// 用例锁定这是**显式 opt-in** 而非空筛选的默认行为（上一子测试锁定默认）。
		g, rec := newCaptureDB(t)
		repo := NewFileCodeRepository(g)
		live := &model.FileCode{Code: "FCALLIV1", Status: model.StatusNormal}
		dead := &model.FileCode{Code: "FCALDEA2", Status: model.StatusNormal}
		require.NoError(t, repo.Create(ctx, live))
		require.NoError(t, repo.Create(ctx, dead))
		require.NoError(t, repo.Delete(ctx, dead.ID))

		_, total, err := repo.ListWithFilter(ctx, model.FileCodeQuery{Deleted: "all", Page: 1, PageSize: 20})
		require.NoError(t, err)
		assert.Equal(t, int64(2), total, "Deleted=all 显式含已删（admin 明示的全量语义）")

		require.NotEmpty(t, *rec)
		for _, r := range *rec {
			assert.NotContains(t, r.SQL, "deleted_at", "Deleted=all 不得暗中带软删条件: %s", r.SQL)
		}
	})

	t.Run("TransferLog.List_空筛选仍带软删约束", func(t *testing.T) {
		g, rec := newCaptureDB(t)
		logRepo := NewTransferLogRepository(g)
		live := &model.TransferLog{Operation: "upload", FileCode: "TLLIVE01"}
		require.NoError(t, logRepo.Create(ctx, live))
		dead := &model.TransferLog{Operation: "upload", FileCode: "TLDEAD02"}
		require.NoError(t, logRepo.Create(ctx, dead))
		require.NoError(t, g.Where("id = ?", dead.ID).Delete(&model.TransferLog{}).Error) // 软删

		items, total, err := logRepo.List(ctx, model.TransferLogQuery{Page: 1, PageSize: 20})
		require.NoError(t, err)
		assert.Equal(t, int64(1), total, "空筛选必须带软删约束（已删日志不得出现）")
		require.Len(t, items, 1)
		assert.Equal(t, "TLLIVE01", items[0].FileCode)

		require.NotEmpty(t, *rec)
		for _, r := range *rec {
			assert.Contains(t, r.SQL, "`transfer_logs`.`deleted_at` IS NULL", "SELECT 必须携带软删约束: %s", r.SQL)
		}
	})
}

// TestQueriesSpecPilot_FailClosed_InvalidConditionFailsClosed crud 层的
// fail-closed 语义在本仓试点上的仓内投影：白名单外的字段/操作符渲染为
// 恒假 1=0 而不是被静默丢弃（静默丢弃 = 租户/权限过滤凭空消失=数据暴露）。
// 直测构建器，钉住「我们依赖的正是这个语义」（上游 TestQuerySpec_FailClosed）。
func TestQueriesSpecPilot_FailClosed_InvalidConditionFailsClosed(t *testing.T) {
	spec := crud.NewQuery().
		Where("user_id", "=", 1).
		Where("user_id", "; DROP TABLE users; --", 1). // 非法操作符
		Where("1=1) OR (1", "=", 1).                   // 非法字段名（注入形状）
		Spec()
	require.Len(t, spec.Conds, 3, "非法条件必须保留为 fail-closed 条件，不得丢弃")
	assert.Equal(t, "user_id = ?", spec.Conds[0].Query)
	assert.Equal(t, "1=0", spec.Conds[1].Query, "未知操作符渲染恒假，绝不静默丢弃")
	assert.Equal(t, "1=0", spec.Conds[2].Query, "白名单外字段名渲染恒假，绝不静默丢弃")
	for i, c := range spec.Conds[1:] {
		assert.Empty(t, c.Args, "fail-closed 条件不携带参数 [%d]", i+1)
	}
}

// TestQueriesSpecPilot_PaginationUntouched 分页口径零变化的行为锚：
// QuerySpec 化之后 LIMIT 归一化结果与手拼时代一致（用户分享上限 100 /
// 管理台与日志通用 200；normalizePage 在 spec 构建前生效，互不影响）。
func TestQueriesSpecPilot_PaginationUntouched(t *testing.T) {
	ctx := context.Background()
	uid := uint(3)

	seed := func(t *testing.T) (*FileCodeRepository, *TransferLogRepository, *[]sqlRec) {
		g, rec := newCaptureDB(t)
		repo := NewFileCodeRepository(g)
		logRepo := NewTransferLogRepository(g)
		for i := 0; i < 25; i++ {
			require.NoError(t, repo.Create(ctx, &model.FileCode{
				Code: fmt.Sprintf("PGN%04d", i), UserID: &uid, ExpiredCount: -1,
			}))
		}
		return repo, logRepo, rec
	}

	t.Run("用户分享默认20条", func(t *testing.T) {
		repo, _, rec := seed(t)
		items, total, err := repo.GetUserSharesWithFilter(ctx, uid, UserShareFilter{Page: 1})
		require.NoError(t, err)
		assert.Equal(t, int64(25), total)
		assert.Len(t, items, 20, "缺省 pageSize=20（UserSharePolicy 口径）")
		for _, r := range *rec {
			if strings.Contains(r.SQL, "SELECT *") {
				assert.Contains(t, r.SQL, "LIMIT 20", "Find 语句 LIMIT=20: %s", r.SQL)
			}
		}
	})

	t.Run("用户分享超限截断到100", func(t *testing.T) {
		repo, _, rec := seed(t)
		_, total, err := repo.GetUserSharesWithFilter(ctx, uid, UserShareFilter{Page: 1, PageSize: 500})
		require.NoError(t, err)
		assert.Equal(t, int64(25), total)
		for _, r := range *rec {
			if strings.Contains(r.SQL, "SELECT *") {
				assert.Contains(t, r.SQL, "LIMIT 100", "500 被截断为上限 100: %s", r.SQL)
				assert.NotContains(t, r.SQL, "LIMIT 500")
			}
		}
	})

	t.Run("管理台通用口径截断到200", func(t *testing.T) {
		repo, _, rec := seed(t)
		_, total, err := repo.ListWithFilter(ctx, model.FileCodeQuery{UserID: &uid, Page: 1, PageSize: 9999})
		require.NoError(t, err)
		assert.Equal(t, int64(25), total)
		for _, r := range *rec {
			if strings.Contains(r.SQL, "SELECT *") {
				assert.Contains(t, r.SQL, "LIMIT 200", "9999 被截断为上限 200: %s", r.SQL)
				assert.NotContains(t, r.SQL, "LIMIT 9999")
			}
		}
	})

	t.Run("日志通用口径截断到200", func(t *testing.T) {
		_, logRepo, rec := seed(t)
		_, total, err := logRepo.List(ctx, model.TransferLogQuery{Page: 1, PageSize: 9999})
		require.NoError(t, err)
		assert.Equal(t, int64(0), total)
		for _, r := range *rec {
			if strings.Contains(r.SQL, "SELECT *") {
				assert.Contains(t, r.SQL, "LIMIT 200", "9999 被截断为上限 200: %s", r.SQL)
				assert.NotContains(t, r.SQL, "LIMIT 9999")
			}
		}
	})
}
