// custom_code_storm_test.go 重复请求风暴：同一自定义取件码被并发重放（L5 层）
//
// ## 背景
//
// 自定义取件码是「创建分享」这条链上**唯一的幂等键**：同一个码只能落一条分享。
// 实现是典型的 **check-then-act + 唯一索引兜底**（app/share/service.go 的
// createWithCode）：
//
//	CheckCodeOccupancy  预检占用（含回收站软删行）⇒ 已被占 ⇒ 直接报文案
//	fileCodeRepo.Create 落库；撞唯一索引 ⇒ gormx.IsUniqueViolation ⇒
//	                    「自定义取件码已被占用，请换一个」
//
// 预检本身**不是**裁决者（两个并发请求可以同时「查不到」⇒ 同时放行）⇒ 真正
// 兜底的是 file_codes.code 上的唯一索引。
//
// ## 缺陷形态
//
// 若唯一索引这道兜底失效（模型上少一个 uniqueIndex、或落库错误被当成成功吞掉）
// ⇒ 并发重放会落**多条**同码分享 ⇒ 一个取件码指向多份内容 ⇒ 取件语义错乱。
// 另一侧缺陷：唯一冲突被当普通错误上抛 ⇒ 用户看到裸 SQL 文本（UNIQUE
// constraint failed: file_codes.code）而不是可操作文案。
//
// ## 断言口径
//
//   - N 个并发重放 ⇒ **恰好 1 个**成功，其余全部失败；
//   - 失败文案必须含「已被占用」（可操作文案），且**不得**漏出 SQL 细节；
//   - file_codes 表里该码**恰 1 行**（副作用只发生一次）。
//
// ## 为什么用临时文件 SQLite
//
// 既有 app/share 测试的 newTestDB 用 :memory: + SetMaxOpenConns(1)（每条连接
// 是独立库，必须钉死单连接）⇒ DB 访问被串行化 ⇒ 碰不到「预检同时放行 ⇒
// 一起撞唯一索引」这条真实并发语义。本文件自建临时文件库（多连接 + WAL +
// busy_timeout）⇒ 真并发。
//
// ## 负对照（必须能变红）
//
// 去掉 model.FileCode.Code 上的 uniqueIndex ⇒ 并发重放落多条同码分享 ⇒
// 「恰 1 行」「恰好 1 个成功」双红；或把 createWithCode 里的
// gormx.IsUniqueViolation 分支去掉 ⇒ 裸 SQL 文案漏出 ⇒ 文案断言红。
package share

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newStormService 构造一个**真并发**的 share service（临时文件 SQLite + 多连接）。
func newStormService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "storm.db") +
		"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"
	gormDB, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{}, &model.PickupCharge{}))
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(8)
	t.Cleanup(func() { _ = sqlDB.Close() })

	svc := NewService(gormDB, "http://localhost:12345", newMockStorage())
	return svc, gormDB
}

// TestCreateWithCode_ConcurrentSameCodeStorm 同一自定义取件码被并发重放 N 次。
//
// 断言：恰好 1 个成功；其余全部失败且文案可操作（不得漏 SQL 细节）；表里恰 1 行。
func TestCreateWithCode_ConcurrentSameCodeStorm(t *testing.T) {
	svc, gormDB := newStormService(t)
	ctx := context.Background()
	const customCode = "Storm01"

	build := func(code string) *model.FileCode {
		return &model.FileCode{Code: code, Status: model.StatusNormal, ExpiredCount: -1}
	}

	const n = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	oks := make([]bool, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 同步起跑：避免 goroutine 逐个启动把并发退化成串行
			_, err := svc.createWithCode(ctx, customCode, build)
			oks[i] = err == nil
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	succeeded := 0
	for i, ok := range oks {
		if ok {
			succeeded++
			continue
		}
		require.Error(t, errs[i], "第 %d 个重放：失败必须有错误", i)
		msg := errs[i].Error()
		assert.Contains(t, msg, "已被占用",
			"第 %d 个重放：必须是可操作文案（实际：%q）", i, msg)
		// 不得把驱动/SQL 细节漏给用户（core#112 的既有口径：裸 SQL 错误不可见）。
		for _, leak := range []string{"UNIQUE constraint", "constraint failed", "SQLITE", "sqlite"} {
			assert.NotContains(t, msg, leak,
				"第 %d 个重放：文案漏出 SQL 细节（%q）", i, leak)
		}
	}
	assert.Equal(t, 1, succeeded,
		"同一自定义码并发重放：必须**恰好 1 个**成功（副作用只发生一次）")

	var rows int64
	require.NoError(t, gormDB.Model(&model.FileCode{}).
		Where("code = ?", customCode).Count(&rows).Error)
	assert.Equal(t, int64(1), rows, "file_codes 表里该码**恰 1 行**（唯一索引兜底的可见证据）")
}

// TestCreateWithCode_ConcurrentSameCodeStorm_NoSQLLeakOnBusyDB 风暴下的错误分类。
//
// 补充断言：并发下失败的请求**必须**是「已被占用」这一类（唯一冲突兜底），
// 而不是 SQLITE_BUSY / "database is locked" 之类的基建错误——后者说明写锁争用
// 没被正确归一，对用户是噪声。
func TestCreateWithCode_ConcurrentSameCodeStorm_NoSQLLeakOnBusyDB(t *testing.T) {
	svc, gormDB := newStormService(t)
	ctx := context.Background()
	const customCode = "Storm02"

	build := func(code string) *model.FileCode {
		return &model.FileCode{Code: code, Status: model.StatusNormal, ExpiredCount: -1}
	}

	const n = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := svc.createWithCode(ctx, customCode, build)
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	okCount, occupied, infra := 0, 0, 0
	for i, err := range errs {
		if err == nil {
			okCount++
			continue
		}
		msg := err.Error()
		switch {
		case strings.Contains(msg, "已被占用"):
			occupied++
		case strings.Contains(msg, "locked") || strings.Contains(msg, "busy"):
			infra++
			t.Errorf("第 %d 个重放：写锁争用未归一为业务错误：%q", i, msg)
		default:
			infra++
			t.Errorf("第 %d 个重放：出现非预期的错误分类：%q", i, msg)
		}
	}
	assert.Equal(t, 1, okCount, "恰好 1 个成功")
	assert.Equal(t, n-1, occupied, "其余必须全部归一为「已被占用」")
	assert.Zero(t, infra, "不得出现基建类错误")

	var rows int64
	require.NoError(t, gormDB.Model(&model.FileCode{}).
		Where("code = ?", customCode).Count(&rows).Error)
	assert.Equal(t, int64(1), rows)
}
