package share

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/app/moderation"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// =====================================================================
// share service 单测
//
// 测试策略（注入式：构造直传 *gorm.DB，dao 去全局化终态）：
//   - storage 用 hand-rolled mock（实现 storage.StorageInterface）
//   - userService / notifySvc 用 hand-rolled mock（实现对应接口）
// =====================================================================

// newTestDB 构造内存 sqlite + AutoMigrate FileCode。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{}))
	// glebarez/sqlite 的 :memory: 每条连接是独立库，多连接会拿到无表空库；
	// 钉死单连接消除该 flake（异步 goroutine 与主流程并发取连接时必现）。
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return gormDB
}

// newTestServiceWithDB 同 newTestService，额外返回测试库句柄（测试直写记录用）。
func newTestServiceWithDB(t *testing.T) (*Service, *gorm.DB, *mockStorage, *mockUserService, *mockNotifyService) {
	t.Helper()
	gormDB := newTestDB(t)
	st := newMockStorage()
	usr := newMockUserService()
	notify := newMockNotifyService()
	svc := NewService(gormDB, "http://localhost:12345", st)
	svc.SetUserService(usr)
	svc.SetNotifyService(notify)
	return svc, gormDB, st, usr, notify
}

// newTestService 构造注入 mock 依赖的 share service。
func newTestService(t *testing.T) (*Service, *mockStorage, *mockUserService, *mockNotifyService) {
	t.Helper()
	svc, _, st, usr, notify := newTestServiceWithDB(t)
	return svc, st, usr, notify
}

// ===== mock: storage.StorageInterface =====

type mockStorage struct {
	deletedPath  string
	deleteCalled bool
	deleteErr    error
	// deletedPaths 累计全部删除路径（多文件子文件删除断言用）
	deletedPaths []string
	// readerBytes GetFileReader 返回内容（zip 流测试用；每次调用返回新流）
	readerBytes []byte
}

func newMockStorage() *mockStorage { return &mockStorage{} }

func (m *mockStorage) SaveFile(_ context.Context, _ *multipart.FileHeader, _ string) (*storage.FileOperationResult, error) {
	return &storage.FileOperationResult{}, nil
}
func (m *mockStorage) DeleteFile(_ context.Context, path string) error {
	m.deleteCalled = true
	m.deletedPath = path
	m.deletedPaths = append(m.deletedPaths, path)
	return m.deleteErr
}
func (m *mockStorage) GetFile(_ context.Context, _ string) ([]byte, error) {
	return nil, nil
}
func (m *mockStorage) FileExists(_ context.Context, _ string) bool { return false }
func (m *mockStorage) GetFileSize(_ context.Context, _ string) (int64, error) {
	return 0, nil
}
func (m *mockStorage) GetFileURL(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (m *mockStorage) GetFileReader(_ context.Context, _ string) (io.ReadCloser, int64, error) {
	if m.readerBytes == nil {
		return nil, 0, nil
	}
	return io.NopCloser(bytes.NewReader(m.readerBytes)), int64(len(m.readerBytes)), nil
}
func (m *mockStorage) SaveStream(_ context.Context, _ string, _ io.Reader, _ int64) (int64, error) {
	return 0, nil
}
func (m *mockStorage) SaveChunk(_ context.Context, _ string, _ int, _ []byte) error {
	return nil
}
func (m *mockStorage) MergeChunks(_ context.Context, _ string, _ int, _ string) error {
	return nil
}
func (m *mockStorage) CleanChunks(_ context.Context, _ string) error { return nil }

// ===== mock: UserServiceInterface =====

type mockUserService struct {
	uploadsCalls int64
	storageDelta int64
	uploadCap    int64
}

func newMockUserService() *mockUserService { return &mockUserService{} }

func (m *mockUserService) UpdateUserStats(_ uint, statsType string, value int64) error {
	switch statsType {
	case "uploads":
		m.uploadsCalls += value
	case "storage":
		m.storageDelta += value
	}
	return nil
}

func (m *mockUserService) GetUploadSizeCap(_ context.Context, _ uint) int64 {
	return m.uploadCap
}

// ===== mock: NotifyServiceInterface =====

type mockNotifyService struct {
	called     int
	lastUserID uint
}

func newMockNotifyService() *mockNotifyService { return &mockNotifyService{} }

func (m *mockNotifyService) CreateForUserSimple(_ context.Context, userID uint, _, _, _, _ string) error {
	m.called++
	m.lastUserID = userID
	return nil
}

// =====================================================================
// 测试用例
// =====================================================================

// 测试：分享文本成功，记录写入 DB
func TestShareText_Success(t *testing.T) {
	svc, _, usr, _ := newTestService(t)
	uid := uint(1)

	resp, err := svc.ShareText(context.Background(), &ShareTextReq{
		Text:         "hello",
		ExpiredCount: -1, // -1 = 无限次数，否则 0 视为已过期
		UserID:       &uid,
		UploadType:   "authenticated",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.Code)
	assert.Equal(t, "hello", resp.Text)
	assert.Equal(t, "authenticated", resp.UploadType)
	// 用户上传统计应 +1
	assert.Equal(t, int64(1), usr.uploadsCalls)

	// DB 应能查回
	fc, err := svc.GetFileByCode(context.Background(), resp.Code)
	require.NoError(t, err)
	assert.Equal(t, "hello", fc.Text)
}

// 测试：取过期分享（ExpiredCount=0 视为已用完）
func TestGetFileByCode_Expired(t *testing.T) {
	svc, _, _, _ := newTestService(t)

	resp, err := svc.ShareText(context.Background(), &ShareTextReq{
		Text:         "expiring",
		ExpiredCount: 0, // 0 → IsExpired()=true
	})
	require.NoError(t, err)

	_, err = svc.GetFileByCode(context.Background(), resp.Code)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "expired")
}

// 测试：取不存在的分享
func TestGetFileByCode_NotFound(t *testing.T) {
	svc, _, _, _ := newTestService(t)

	_, err := svc.GetFileByCode(context.Background(), "NOPE1234")
	assert.Error(t, err)
}

// 测试：按次数过期——UpdateFileUsage 原子递减 ExpiredCount
func TestUpdateFileUsage_DecrementCount(t *testing.T) {
	svc, _, _, _ := newTestService(t)

	resp, err := svc.ShareText(context.Background(), &ShareTextReq{
		Text:         "counted",
		ExpiredCount: 2,
	})
	require.NoError(t, err)

	// 第一次取件：2 → 1
	ok, err := svc.UpdateFileUsage(context.Background(), resp.Code)
	require.NoError(t, err)
	assert.True(t, ok)
	fc, err := svc.GetFileByCode(context.Background(), resp.Code)
	require.NoError(t, err)
	assert.Equal(t, 1, fc.ExpiredCount)
	assert.Equal(t, 1, fc.UsedCount)

	// 第二次取件：1 → 0（此时变为已过期）
	ok2, err := svc.UpdateFileUsage(context.Background(), resp.Code)
	require.NoError(t, err)
	assert.True(t, ok2)
	// ExpiredCount=0 → IsExpired()=true → GetFileByCode 报错
	fc2, err := svc.GetFileByCode(context.Background(), resp.Code)
	assert.Error(t, err)
	assert.Nil(t, fc2)

	// 第三次：已耗尽，ok=false
	ok3, err := svc.UpdateFileUsage(context.Background(), resp.Code)
	require.NoError(t, err)
	assert.False(t, ok3)
}

// 测试：无限次数分享（ExpiredCount=-1）可反复取件
func TestUpdateFileUsage_Unlimited(t *testing.T) {
	svc, _, _, _ := newTestService(t)

	resp, err := svc.ShareText(context.Background(), &ShareTextReq{
		Text:         "unlimited",
		ExpiredCount: -1,
	})
	require.NoError(t, err)

	for i := 0; i < 5; i++ {
		ok, err := svc.UpdateFileUsage(context.Background(), resp.Code)
		require.NoError(t, err)
		assert.True(t, ok)
	}
	fc, err := svc.GetFileByCode(context.Background(), resp.Code)
	require.NoError(t, err)
	assert.Equal(t, -1, fc.ExpiredCount) // 不递减
	assert.Equal(t, 5, fc.UsedCount)
}

// 测试：GetFileWithUsage 真实密码校验
func TestGetFileWithUsage_PasswordCheck(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	hash, err := utils.HashPassword("rightpwd")
	require.NoError(t, err)

	resp, err := svc.CreateShare(ctx, &ShareFileReq{
		FilePath: "a/b", Size: 1, ExpiredCount: -1,
		RequireAuth: true, PasswordHash: hash,
	})
	require.NoError(t, err)

	// 错误密码 → 报错
	_, err = svc.GetFileWithUsage(ctx, resp.Code, "wrongpwd", "1.2.3.4", false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "密码错误")

	// 正确密码 → 成功
	fc, err := svc.GetFileWithUsage(ctx, resp.Code, "rightpwd", "1.2.3.4", false)
	require.NoError(t, err)
	assert.Equal(t, resp.Code, fc.Code)

	// authedByToken=true（有效下载令牌）等价已认证：空密码也应放行
	_, err = svc.GetFileWithUsage(ctx, resp.Code, "", "1.2.3.4", true)
	require.NoError(t, err, "持有效下载令牌（取件校验通过后签发）应视为已认证")
}

// 测试：按时间过期——ExpiredAt 在过去
func TestGetFileByCode_ExpiredByTime(t *testing.T) {
	svc, _, _, _ := newTestService(t)

	past := time.Now().Add(-1 * time.Hour)
	resp, err := svc.ShareText(context.Background(), &ShareTextReq{
		Text:         "past",
		ExpiredAt:    &past,
		ExpiredCount: -1, // 次数无限，但时间已过
	})
	require.NoError(t, err)

	_, err = svc.GetFileByCode(context.Background(), resp.Code)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "expired")
}

// 测试：删除分享——所有权校验（非 owner 拒绝）
func TestDeleteFileByCode_OwnershipDenied(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	owner := uint(1)

	resp, err := svc.ShareText(context.Background(), &ShareTextReq{
		Text:         "mine",
		ExpiredCount: -1,
		UserID:       &owner,
	})
	require.NoError(t, err)

	// 另一个用户尝试删除 → 拒绝
	err = svc.DeleteFileByCode(context.Background(), resp.Code, 999)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "无权限")
}

// 测试：删除分享——owner 删除成功
func TestDeleteFileByCode_OwnerSuccess(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	owner := uint(1)

	resp, err := svc.ShareText(context.Background(), &ShareTextReq{
		Text:         "mine",
		ExpiredCount: -1,
		UserID:       &owner,
	})
	require.NoError(t, err)

	require.NoError(t, svc.DeleteFileByCode(context.Background(), resp.Code, owner))
	// 软删除后查不到
	_, err = svc.GetFileByCode(context.Background(), resp.Code)
	assert.Error(t, err)
}

// 测试：GenerateCode 生成 8 位非空码，且多次不重复
func TestGenerateCode(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	c1 := svc.GenerateCode()
	c2 := svc.GenerateCode()
	assert.Len(t, c1, 8)
	assert.Len(t, c2, 8)
	assert.NotEqual(t, c1, c2)
}

// 测试：GenerateCode 1000 次内不重复（crypto/rand 质量）
func TestGenerateCode_UniqueHighVolume(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		c := svc.GenerateCode()
		assert.Len(t, c, 8)
		assert.False(t, seen[c], "1000 次内不应重复: %s", c)
		seen[c] = true
	}
}

// 测试：CreateShare 正常路径（含 PasswordHash）
func TestCreateShare_WithPassword(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	resp, err := svc.CreateShare(context.Background(), &ShareFileReq{
		FilePath: "x/y", Size: 10, ExpiredCount: -1,
		RequireAuth: true, PasswordHash: "$2a$10$dummyhash",
	})
	require.NoError(t, err)
	assert.Len(t, resp.Code, 8)
	assert.True(t, resp.RequireAuth)

	// DB 里确实写入了
	fc, err := svc.GetFileByCode(context.Background(), resp.Code)
	require.NoError(t, err)
	assert.Equal(t, "$2a$10$dummyhash", fc.PasswordHash)
}

// TestIsTextShare 回归（P0）：ShareFile 把原始文件名存进 Text 字段，
// 下载端点曾因仅凭 Text != "" 判定文本分享，把文件分享的下载拦截成
// "返回文件名字符串"。正确判定 = Text 非空且无文件路径。
func TestIsTextShare(t *testing.T) {
	fileNameInText := &model.FileCode{Text: "report.pdf", FilePath: "uploads/2026/10/03/uuid.pdf"}
	assert.False(t, IsTextShare(fileNameInText), "文件分享（Text 存的是文件名）不得判为文本分享")

	pureText := &model.FileCode{Text: "hello world", FilePath: ""}
	assert.True(t, IsTextShare(pureText))

	legacyFile := &model.FileCode{Text: "", FilePath: "uploads/x/y.bin"}
	assert.False(t, IsTextShare(legacyFile))

	assert.False(t, IsTextShare(nil))
}

// ---- 治理重构（2026-10-03）：单用户单次上传上限接线 ----

func TestCreateShare_UserUploadSizeCap(t *testing.T) {
	uid := uint(7)

	t.Run("超上限拒绝", func(t *testing.T) {
		svc, _, usr, _ := newTestService(t)
		usr.uploadCap = 100
		_, err := svc.CreateShare(context.Background(), &ShareFileReq{
			FilePath: "a/b", Size: 200, ExpiredCount: -1, UserID: &uid,
		})
		assert.Error(t, err)
		// 单文件/多文件通道统一走 checkUploadCaps，文案单一真相（"总大小"对单文件即文件大小）
		assert.Contains(t, err.Error(), "上传总大小超过限制")
	})
	t.Run("未超上限放行", func(t *testing.T) {
		svc, _, usr, _ := newTestService(t)
		usr.uploadCap = 100
		resp, err := svc.CreateShare(context.Background(), &ShareFileReq{
			FilePath: "a/b", Size: 50, ExpiredCount: -1, UserID: &uid,
		})
		assert.NoError(t, err)
		assert.NotEmpty(t, resp.Code)
	})
	t.Run("匿名不受限", func(t *testing.T) {
		svc, _, usr, _ := newTestService(t)
		usr.uploadCap = 1
		resp, err := svc.CreateShare(context.Background(), &ShareFileReq{
			FilePath: "a/b", Size: 999, ExpiredCount: -1,
		})
		assert.NoError(t, err)
		assert.NotEmpty(t, resp.Code)
	})
	t.Run("上限0不限", func(t *testing.T) {
		svc, _, usr, _ := newTestService(t)
		usr.uploadCap = 0
		resp, err := svc.CreateShare(context.Background(), &ShareFileReq{
			FilePath: "a/b", Size: 999999, ExpiredCount: -1, UserID: &uid,
		})
		assert.NoError(t, err)
		assert.NotEmpty(t, resp.Code)
	})
}

// ---- 治理重构（2026-10-03）：状态机 + 取件拒绝 ----

func TestShareStatusMachine(t *testing.T) {
	t.Run("blocked取件拒绝且业务码20012", func(t *testing.T) {
		svc, _, _, _ := newTestService(t)
		ctx := context.Background()
		resp, err := svc.CreateShare(ctx, &ShareFileReq{FilePath: "a/b", Size: 1, ExpiredCount: -1})
		require.NoError(t, err)

		n, err := svc.SetShareStatus(ctx, []uint{resp.ID}, model.StatusBlocked)
		require.NoError(t, err)
		assert.Equal(t, int64(1), n)

		_, err = svc.GetFileByCode(ctx, resp.Code)
		require.Error(t, err)
		var blocked *ShareBlockedError
		require.True(t, errors.As(err, &blocked))
		assert.Equal(t, 20012, blocked.ErrCode())

		// 恢复后可取件
		_, err = svc.SetShareStatus(ctx, []uint{resp.ID}, model.StatusNormal)
		require.NoError(t, err)
		_, err = svc.GetFileByCode(ctx, resp.Code)
		assert.NoError(t, err)
	})
	t.Run("pending_review业务码20013", func(t *testing.T) {
		svc, _, _, _ := newTestService(t)
		ctx := context.Background()
		resp, err := svc.CreateShare(ctx, &ShareFileReq{FilePath: "a/b", Size: 1, ExpiredCount: -1})
		require.NoError(t, err)
		_, err = svc.SetShareStatus(ctx, []uint{resp.ID}, model.StatusPendingReview)
		require.NoError(t, err)
		_, err = svc.GetFileByCode(ctx, resp.Code)
		var blocked *ShareBlockedError
		require.True(t, errors.As(err, &blocked))
		assert.Equal(t, 20013, blocked.ErrCode())
	})
	t.Run("非法状态被DAO白名单拒绝", func(t *testing.T) {
		svc, _, _, _ := newTestService(t)
		_, err := svc.SetShareStatus(context.Background(), []uint{1}, "hacked")
		assert.Error(t, err)
	})
}

// ---- 治理重构（2026-10-03）：内容审核钩子 ----

type mockModerator struct{ verdict moderation.Verdict }

func (m *mockModerator) InspectText(context.Context, string) moderation.Verdict { return m.verdict }
func (m *mockModerator) InspectFile(context.Context, moderation.UploadMeta) moderation.Verdict {
	return moderation.VerdictAllow
}

type mockFlagEmitter struct{ codes []string }

func (m *mockFlagEmitter) EmitShareFlagged(code, reason, ownerIP string) {
	m.codes = append(m.codes, code)
}

func TestShareText_Moderation(t *testing.T) {
	t.Run("reject 拦截返回30013", func(t *testing.T) {
		svc, _, _, _ := newTestService(t)
		svc.SetModerator(&mockModerator{verdict: moderation.VerdictReject})
		_, err := svc.ShareTextWithAuth(context.Background(), "some text", 1, "day", false, "", nil, "1.1.1.1", false, "")
		require.Error(t, err)
		var cre *ContentRejectedError
		require.True(t, errors.As(err, &cre))
		assert.Equal(t, 30013, cre.ErrCode())
	})
	t.Run("pending 建分享后置待审+事件", func(t *testing.T) {
		svc, _, _, _ := newTestService(t)
		emitter := &mockFlagEmitter{}
		svc.SetModerator(&mockModerator{verdict: moderation.VerdictPending})
		svc.SetFlagEventEmitter(emitter)
		resp, err := svc.ShareTextWithAuth(context.Background(), "some text", 1, "day", false, "", nil, "1.1.1.1", false, "")
		require.NoError(t, err)
		assert.Equal(t, model.StatusPendingReview, resp.Status)
		assert.Len(t, emitter.codes, 1)

		// 待审分享取件被拒
		_, err = svc.GetFileByCode(context.Background(), resp.Code)
		var blocked *ShareBlockedError
		require.True(t, errors.As(err, &blocked))
		assert.Equal(t, 20013, blocked.ErrCode())
	})
	t.Run("allow 正常且无事件", func(t *testing.T) {
		svc, _, _, _ := newTestService(t)
		emitter := &mockFlagEmitter{}
		svc.SetModerator(&mockModerator{verdict: moderation.VerdictAllow})
		svc.SetFlagEventEmitter(emitter)
		resp, err := svc.ShareTextWithAuth(context.Background(), "some text", 1, "day", false, "", nil, "1.1.1.1", false, "")
		require.NoError(t, err)
		assert.Equal(t, model.StatusNormal, resp.Status)
		assert.Empty(t, emitter.codes)
	})
}
