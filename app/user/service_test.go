package user

import (
	"context"
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/auth"
	pkgerrors "github.com/pigeonbox/core/pkg/errors"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// =====================================================================
// user service 单测（注入式：构造直传 *gorm.DB，dao 去全局化终态）
// =====================================================================

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&model.User{}, &model.UserAPIKey{}))
	// glebarez/sqlite 的 :memory: 每条连接是独立库，多连接会拿到无表空库；
	// 钉死单连接消除该 flake（异步 goroutine 与主流程并发取连接时必现）。
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return gormDB
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	gormDB := newTestDB(t)
	// 登录会生成 JWT，需要设置 secret（避免使用硬编码默认值）
	auth.SetJWTSecret("test-secret-for-user-service")
	return NewService(gormDB)
}

// 测试：创建用户成功
func TestCreate_Success(t *testing.T) {
	svc := newTestService(t)
	resp, err := svc.Create(context.Background(), &CreateUserReq{
		Username: "alice",
		Email:    "alice@example.com",
		Password: "passw0rd!",
		Nickname: "Alice",
	})
	require.NoError(t, err)
	assert.Equal(t, "alice", resp.Username)
	assert.Equal(t, "alice@example.com", resp.Email)
	assert.Equal(t, "Alice", resp.Nickname)
}

// 测试：非法用户名 ⇒ typed 参数错误（CodeBadRequest），不是裸 error。
// 负对照：把 Create 里 ValidateUsername 的 pkgerrors.BadRequest 包装去掉
// ⇒ 本用例红（errors.As 接不到业务错误）。
func TestCreate_InvalidUsername_TypedBadRequest(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.Create(context.Background(), &CreateUserReq{
		Username: "bad name!", Email: "x@example.com", Password: "x",
	})
	require.Error(t, err)
	var bizErr *pkgerrors.AppError
	require.True(t, errors.As(err, &bizErr), "非法用户名应返回 typed 业务错误：%v", err)
	assert.Equal(t, pkgerrors.CodeBadRequest, bizErr.BizCode)
}

// 测试：重复用户名拒绝
func TestCreate_DuplicateUsername(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.Create(context.Background(), &CreateUserReq{
		Username: "bob", Email: "bob1@example.com", Password: "x",
	})
	require.NoError(t, err)

	_, err = svc.Create(context.Background(), &CreateUserReq{
		Username: "bob", Email: "bob2@example.com", Password: "x",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	// sentinel 锁定：撞用户名必须是 ErrUsernameExists（typed 业务错误），
	// handler 才能映射 409 而不是 500（2026-11-02 注册撞名 500 修复）。
	// 负对照：换回 errors.New 裸错误 ⇒ 本断言红。
	assert.ErrorIs(t, err, ErrUsernameExists)
}

// 测试：重复邮箱拒绝
func TestCreate_DuplicateEmail(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.Create(context.Background(), &CreateUserReq{
		Username: "carol", Email: "carol@example.com", Password: "x",
	})
	require.NoError(t, err)

	_, err = svc.Create(context.Background(), &CreateUserReq{
		Username: "carol2", Email: "carol@example.com", Password: "x",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	// sentinel 锁定：撞邮箱必须是 ErrEmailExists（负对照同上）。
	assert.ErrorIs(t, err, ErrEmailExists)
}

// 测试：登录成功返回 JWT token
func TestLogin_Success(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.Create(context.Background(), &CreateUserReq{
		Username: "dave", Email: "dave@example.com", Password: "secret123",
	})
	require.NoError(t, err)

	resp, token, err := svc.Login(context.Background(), "dave", "secret123")
	require.NoError(t, err)
	assert.Equal(t, "dave", resp.Username)
	assert.NotEmpty(t, token, "应返回 JWT token")

	// token 应可解析
	claims, err := auth.ParseToken(token)
	require.NoError(t, err)
	assert.Equal(t, "dave", claims.Username)
}

// 测试：密码错误
func TestLogin_WrongPassword(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.Create(context.Background(), &CreateUserReq{
		Username: "eve", Email: "eve@example.com", Password: "correct",
	})
	require.NoError(t, err)

	_, _, err = svc.Login(context.Background(), "eve", "wrong")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "密码错误")
}

// 测试：登录不存在的用户
func TestLogin_UserNotFound(t *testing.T) {
	svc := newTestService(t)
	_, _, err := svc.Login(context.Background(), "ghost", "whatever")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "密码错误") // 不泄露用户是否存在
}

// 测试：GetByID
func TestGetByID(t *testing.T) {
	svc := newTestService(t)
	created, err := svc.Create(context.Background(), &CreateUserReq{
		Username: "frank", Email: "frank@example.com", Password: "x",
	})
	require.NoError(t, err)

	got, err := svc.GetByID(context.Background(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, "frank", got.Username)
}

// 测试：GetByID 不存在
func TestGetByID_NotFound(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.GetByID(context.Background(), 9999)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// 测试：删除用户
func TestDelete(t *testing.T) {
	svc := newTestService(t)
	created, err := svc.Create(context.Background(), &CreateUserReq{
		Username: "grace", Email: "grace@example.com", Password: "x",
	})
	require.NoError(t, err)

	require.NoError(t, svc.Delete(context.Background(), created.ID))
	_, err = svc.GetByID(context.Background(), created.ID)
	assert.Error(t, err)
}

// ---- 治理重构（2026-10-03）：GetUploadSizeCap 单用户单次上传上限 ----

func TestGetUploadSizeCap(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	t.Run("用户未设置回退系统默认", func(t *testing.T) {
		old := conf.GetGlobalConfig()
		conf.SetGlobalConfig(&conf.AppConfiguration{User: conf.UserConfig{UserUploadSize: 52428800}})
		t.Cleanup(func() { conf.SetGlobalConfig(old) })
		u, err := svc.Create(ctx, &CreateUserReq{Username: "cap1", Email: "cap1@example.com", Password: "x"})
		require.NoError(t, err)
		cap1 := svc.GetUploadSizeCap(ctx, u.ID)
		assert.Equal(t, int64(52428800), cap1)
	})
	t.Run("用户级覆盖", func(t *testing.T) {
		u, err := svc.Create(ctx, &CreateUserReq{Username: "cap2", Email: "cap2@example.com", Password: "x"})
		require.NoError(t, err)
		_, err = svc.SetUploadSize(ctx, u.ID, 1024)
		require.NoError(t, err)
		assert.Equal(t, int64(1024), svc.GetUploadSizeCap(ctx, u.ID))
	})
	t.Run("用户不存在返回0", func(t *testing.T) {
		assert.Equal(t, int64(0), svc.GetUploadSizeCap(ctx, 99999))
	})
}

// =====================================================================
// API Key 撤销时间必须**回传**（2026-10-30 真机实录）
// =====================================================================
//
// 现象：真机查「撤销到底生效没有」时发现 —— DB 里 `user_api_keys.revoked_at`
// **一直有值**（撤销时正确写入），但 `GET /user/api-keys` 的响应里**没有这个字段**
// ⇒ 用户/前端看不到「这把 Key 是什么时候被撤销的」⇒ 审计信息断在**回传面**。
//
// 判据打在**真正会被读到的量**上：ListAPIKeys 返回的结构体里必须带撤销时间
// （不是「撤销成功就算数」——那是另一个量）。
func TestListAPIKeys_IncludesRevokedAt(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	created, err := svc.Create(ctx, &CreateUserReq{
		Username: "kate", Email: "kate@example.com", Password: "passw0rd!",
	})
	require.NoError(t, err)

	key, err := svc.CreateAPIKey(ctx, created.ID, &CreateAPIKeyReq{Name: "probe"})
	require.NoError(t, err)
	require.NotNil(t, key.APIKey)
	assert.Nil(t, key.APIKey.RevokedAt, "新建的 Key 不应有撤销时间")

	require.NoError(t, svc.DeleteAPIKey(ctx, created.ID, key.APIKey.ID))

	list, err := svc.ListAPIKeys(ctx, created.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.True(t, list[0].Revoked)
	require.NotNil(t, list[0].RevokedAt, "已撤销的 Key 必须回传撤销时间（DB 有值但响应不给 = 审计断在回传面）")
}
