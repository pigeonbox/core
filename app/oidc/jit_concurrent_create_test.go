package oidc

import (
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

// TestMatchOrCreate_ConcurrentFirstLoginSingleCreate core#155 正判据：
// 同账号并发首次回调——两路都过 GetByUsername → NotFound 后同时 Create——
// 必须收敛为「恰好一个 Create 成功，另一路重查复用同一行」，绝不 duplicate 上抛 500。
//
// 竞态窗口确定性（不靠调度碰运气）：gorm Query 后置回调把两次 GetByUsername 关进
// 闸门——两个查询都执行完（都看到 0 行）才放行去 Create ⇒ 负对照（回退重查复用）
// 下也恒复现 duplicate。
// 库用文件库 + 4 连接（:memory: 每连接独立库，跨连接不可见，无法复现并发竞争）。
func TestMatchOrCreate_ConcurrentFirstLoginSingleCreate(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "oidc-jit.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	gormDB, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() {
		if sqlDB, e := gormDB.DB(); e == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, gormDB.AutoMigrate(&model.User{}))
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)

	// 计数①：Create 成功次数（正判据要求恰好 1）
	var creates atomic.Int32
	require.NoError(t, gormDB.Callback().Create().After("gorm:create").Register("test:count_create", func(db *gorm.DB) {
		if db.Error == nil {
			creates.Add(1)
		}
	}))
	// 计数②：GetByOIDCSub 查询次数 = 两路初始匹配 2 + 落败一路重查复用 1 = 3
	var oidcSubGets atomic.Int32
	// 闸门：两次 GetByUsername 都执行完才放行 ⇒ 双双看到 0 行后同时 Create
	var usernameGets atomic.Int32
	gate := make(chan struct{})
	require.NoError(t, gormDB.Callback().Query().After("gorm:query").Register("test:username_gate", func(db *gorm.DB) {
		// ⚠️ 不能因 db.Error 跳过：First 未命中时 gorm:query 内已置 ErrRecordNotFound，
		// 跳过 ⇒ 闸门/计数对「未命中」查询全部失灵（判据恒摆设）。
		sql := db.Statement.SQL.String()
		// dao 的 Where 是裸列名（无反引号）：Where("username = ?", ...) ⇒ 匹配裸形态
		if strings.Contains(sql, "oidc_sub = ?") {
			oidcSubGets.Add(1)
		}
		if !strings.Contains(sql, "username = ?") {
			return
		}
		if usernameGets.Add(1) == 2 {
			close(gate)
		}
		select {
		case <-gate:
		case <-time.After(3 * time.Second): // 只有一方到达（退化场景）不挂死
		}
	}))

	svc := NewService(gormDB, Config{
		Enabled:  true,
		Issuer:   "https://idp.example",
		ClientID: "client-1",
	})
	claims := idClaims{
		Sub:               "user-sub-42",
		Email:             "alice@example.com",
		EmailVerified:     true,
		PreferredUsername: "alice",
		Name:              "Alice",
	}

	const workers = 2
	start := make(chan struct{})
	users := make([]*model.User, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			users[i], errs[i] = svc.matchOrCreate(t.Context(), claims)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < workers; i++ {
		require.NoErrorf(t, errs[i], "并发首登第 %d 路不得失败（旧实现 duplicate 直接上抛 ⇒ 500）", i+1)
		require.NotNil(t, users[i])
	}
	assert.EqualValues(t, 1, creates.Load(), "恰好一个 Create 成功")
	assert.Equal(t, users[0].ID, users[1].ID, "落败一路必须重查复用赢家刚建的同一行")
	assert.Equal(t, "alice", users[0].Username, "用户名仍是基础名（先到先得）")
	assert.EqualValues(t, 3, oidcSubGets.Load(), "重查复用走 GetByOIDCSub（2 次初始匹配 + 1 次重查）")

	var count int64
	require.NoError(t, gormDB.Model(&model.User{}).Count(&count).Error)
	assert.EqualValues(t, 1, count, "库里只有一行（不产生重复账号）")
}
