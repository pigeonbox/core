package bootstrap

// 首启令牌**自动生成**（2026-10-30）的行为断言。
//
// 守卫点（四个，缺一即失效）：
//  1. 未初始化 + 未显式配令牌 ⇒ 启动时自动生成一次性令牌（暴露面默认收口）；
//  2. 显式 PB_SETUP_TOKEN 优先 / 自动生成可关闭（PB_SETUP_TOKEN_AUTO=0）；
//  3. 已初始化 ⇒ 不生成（首启面已关闭，令牌无意义）；
//  4. **缺令牌不消耗按 IP 的失败预算**（猜错才消耗）——
//     否则「照着日志填令牌」的那一次点击可能已把预算耗尽
//     ⇒ 明明有正确令牌却被 429 锁在门外。

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/app/setup"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newSetupSvc 造一个未初始化的 setup 服务（内存 SQLite，库中无 admin）。
func newSetupSvc(t *testing.T) *setup.Service {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&model.User{}))
	// glebarez/sqlite :memory: 多连接各为独立库，钉死单连接防 flake
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return setup.NewService(gormDB)
}

// newInitializedSetupSvc 造一个**已初始化**的 setup 服务（库中已有 admin）。
func newInitializedSetupSvc(t *testing.T) *setup.Service {
	t.Helper()
	svc := newSetupSvc(t)
	require.NoError(t, svc.InitializeSystem(context.Background(), &setup.InitializeSystemReq{
		AdminUsername: "admin",
		AdminPassword: "s3cure-Pr0d-Passw0rd!",
		AdminEmail:    "admin@example.com",
	}))
	return svc
}

// resetSetupAutoToken 清掉进程内的一次性令牌（测试隔离用）。
func resetSetupAutoToken(t *testing.T) {
	t.Helper()
	old := setupAutoToken.Load()
	t.Cleanup(func() {
		if old != nil {
			setupAutoToken.Store(old)
			return
		}
		setupAutoToken.Store(nil)
	})
	setupAutoToken.Store(nil)
}

// withSetupSvc 临时替换包级 publicSetupSvc（测试隔离用）。
func withSetupSvc(t *testing.T, svc *setup.Service) {
	t.Helper()
	old := publicSetupSvc
	publicSetupSvc = svc
	t.Cleanup(func() { publicSetupSvc = old })
}

// withTestConfig 临时替换包级 config（测试隔离用）。
func withTestConfig(t *testing.T, cfg *conf.AppConfiguration) {
	t.Helper()
	old := config
	config = cfg
	t.Cleanup(func() { config = old })
}

// withGlobalTestConfig 临时替换**全局 conf**（copy-on-write 快照的真相源）
// 与包级 config（测试隔离用）。
//
// 两者都要换：能力快照里的联邦段读的是全局 conf 快照（管理台保存配置后
// DB overlay 更新的是它），而部署模式等读包级 config ⇒ 只换一半会读到
// 上一个测试残留的值。
func withGlobalTestConfig(t *testing.T, cfg *conf.AppConfiguration) {
	t.Helper()
	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(cfg)
	t.Cleanup(func() { conf.SetGlobalConfig(old) })
	withTestConfig(t, cfg)
}

func TestEnsureSetupToken_GeneratesWhenUninitialized(t *testing.T) {
	resetSetupAutoToken(t)
	t.Setenv(setupTokenEnv, "")
	t.Setenv(setupTokenAutoEnv, "")
	withSetupSvc(t, newSetupSvc(t))
	withTestConfig(t, &conf.AppConfiguration{App: conf.AppConfig{DataPath: t.TempDir()}})

	ensureSetupToken(context.Background())

	token := effectiveSetupToken()
	assert.NotEmpty(t, token, "未初始化且未显式配令牌 ⇒ 必须自动生成一次性令牌")
	assert.Len(t, token, 32, "令牌应为 16 字节 hex（32 字符）")

	// 令牌文件落在数据目录（NAS 用户不必翻日志）
	p := setupTokenFilePath()
	require.NotEmpty(t, p)
	data, err := os.ReadFile(p)
	require.NoError(t, err, "令牌文件必须落盘")
	assert.Equal(t, token, string(data))
	info, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "令牌文件必须是 0600（只有属主可读）")
}

func TestEnsureSetupToken_ExplicitEnvWins(t *testing.T) {
	resetSetupAutoToken(t)
	t.Setenv(setupTokenEnv, "operator-chosen-token")
	t.Setenv(setupTokenAutoEnv, "")
	withSetupSvc(t, newSetupSvc(t))
	withTestConfig(t, &conf.AppConfiguration{App: conf.AppConfig{DataPath: t.TempDir()}})

	ensureSetupToken(context.Background())

	assert.Equal(t, "operator-chosen-token", effectiveSetupToken(),
		"显式 PB_SETUP_TOKEN 优先于自动生成")
}

func TestEnsureSetupToken_AutoCanBeDisabled(t *testing.T) {
	resetSetupAutoToken(t)
	t.Setenv(setupTokenEnv, "")
	t.Setenv(setupTokenAutoEnv, "0")
	withSetupSvc(t, newSetupSvc(t))
	withTestConfig(t, &conf.AppConfiguration{App: conf.AppConfig{DataPath: t.TempDir()}})

	ensureSetupToken(context.Background())

	assert.Empty(t, effectiveSetupToken(), "PB_SETUP_TOKEN_AUTO=0 ⇒ 不得自动生成令牌")
}

func TestEnsureSetupToken_SkippedWhenInitialized(t *testing.T) {
	resetSetupAutoToken(t)
	t.Setenv(setupTokenEnv, "")
	t.Setenv(setupTokenAutoEnv, "")
	withSetupSvc(t, newInitializedSetupSvc(t))
	withTestConfig(t, &conf.AppConfiguration{App: conf.AppConfig{DataPath: t.TempDir()}})

	ensureSetupToken(context.Background())

	assert.Empty(t, effectiveSetupToken(), "已初始化 ⇒ 首启面已关闭，不需要令牌")
}

func TestClearSetupToken_RemovesFileAndValue(t *testing.T) {
	resetSetupAutoToken(t)
	t.Setenv(setupTokenEnv, "")
	t.Setenv(setupTokenAutoEnv, "")
	withSetupSvc(t, newSetupSvc(t))
	dir := t.TempDir()
	withTestConfig(t, &conf.AppConfiguration{App: conf.AppConfig{DataPath: dir}})

	ensureSetupToken(context.Background())
	require.NotEmpty(t, effectiveSetupToken())
	p := setupTokenFilePath()
	require.FileExists(t, p)

	clearSetupToken()

	assert.Empty(t, effectiveSetupToken(), "首启成功后令牌必须失效")
	assert.NoFileExists(t, p, "首启成功后令牌文件必须被清掉")
}

func TestSetupGate_AutoTokenAccepted(t *testing.T) {
	resetSetupAutoToken(t)
	t.Setenv(setupTokenEnv, "")
	t.Setenv(setupTokenAutoEnv, "")
	withSetupSvc(t, newSetupSvc(t))
	withTestConfig(t, &conf.AppConfiguration{App: conf.AppConfig{DataPath: t.TempDir()}})
	ensureSetupToken(context.Background())
	token := effectiveSetupToken()
	require.NotEmpty(t, token)

	reached, code := runSetupGate(t, http.MethodPost, "/setup", token, "")
	assert.True(t, reached, "自动生成的令牌必须被接受")
	assert.Equal(t, http.StatusOK, code)
}

func TestSetupGate_MissingTokenDoesNotConsumeFailureBudget(t *testing.T) {
	resetSetupAutoToken(t)
	t.Setenv(setupTokenEnv, "right-token")
	t.Setenv(setupTokenAutoEnv, "")

	lock := middleware.GetDefaultLockout()
	key := setupLockKey()
	lock.Reset(context.Background(), key)
	t.Cleanup(func() { lock.Reset(context.Background(), key) })

	// 连打 N 次「没带令牌」——这是**正常流程**（前端 401 后才弹输入框），
	// 不得计入失败预算。
	for i := 0; i < 10; i++ {
		reached, code := runSetupGate(t, http.MethodPost, "/setup", "", "")
		assert.False(t, reached, "缺令牌必须被拦下")
		assert.Equal(t, http.StatusUnauthorized, code)
	}

	// 关键断言：预算没被吃掉 ⇒ 带上正确令牌仍然放行（没被 429 锁死）
	reached, code := runSetupGate(t, http.MethodPost, "/setup", "right-token", "")
	assert.True(t, reached, "缺令牌不得消耗失败预算 —— 否则照日志填令牌也会被锁在门外")
	assert.Equal(t, http.StatusOK, code)
}

func TestSetupGate_WrongTokenStillConsumesBudget(t *testing.T) {
	resetSetupAutoToken(t)
	t.Setenv(setupTokenEnv, "right-token")
	t.Setenv(setupTokenAutoEnv, "")

	lock := middleware.GetDefaultLockout()
	key := setupLockKey()
	lock.Reset(context.Background(), key)
	t.Cleanup(func() { lock.Reset(context.Background(), key) })

	// 「提交了但猜错」= 攻击尝试 ⇒ 必须计入失败预算并最终锁定
	locked := false
	for i := 0; i < 20 && !locked; i++ {
		reached, code := runSetupGate(t, http.MethodPost, "/setup", "wrong-token", "")
		assert.False(t, reached, "猜错令牌不得放行")
		if code == http.StatusTooManyRequests {
			locked = true
		}
	}
	assert.True(t, locked, "猜错令牌必须触发按 IP 锁定")

	reached, code := runSetupGate(t, http.MethodPost, "/setup", "right-token", "")
	assert.False(t, reached, "锁定期间即使令牌正确也不得放行")
	assert.Equal(t, http.StatusTooManyRequests, code)
}

func TestSetupTokenFilePath_AbsoluteUnderDataPath(t *testing.T) {
	withTestConfig(t, &conf.AppConfiguration{App: conf.AppConfig{DataPath: "data"}})
	p := setupTokenFilePath()
	assert.True(t, filepath.IsAbs(p), "令牌文件路径必须是绝对路径（相对路径按进程 CWD 解析 ⇒ NAS 部署必错位）")
	assert.Equal(t, ".setup_token", filepath.Base(p))
}
