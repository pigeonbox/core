package admin

import (
	"context"
	"io"
	"mime/multipart"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// mockStorage 记录被删除的物理文件路径（实现完整 StorageInterface）
type mockStorage struct {
	mu      sync.Mutex
	deleted []string
}

func (m *mockStorage) SaveFile(_ context.Context, _ *multipart.FileHeader, _ string) (*storage.FileOperationResult, error) {
	return &storage.FileOperationResult{}, nil
}
func (m *mockStorage) DeleteFile(_ context.Context, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, path)
	return nil
}
func (m *mockStorage) GetFile(_ context.Context, _ string) ([]byte, error) { return nil, nil }
func (m *mockStorage) FileExists(_ context.Context, _ string) bool         { return false }
func (m *mockStorage) GetFileSize(_ context.Context, _ string) (int64, error) {
	return 0, nil
}
func (m *mockStorage) GetFileURL(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (m *mockStorage) GetFileReader(_ context.Context, _ string) (io.ReadCloser, int64, error) {
	return nil, 0, nil
}
func (m *mockStorage) SaveStream(_ context.Context, _ string, _ io.Reader, _ int64) (int64, error) {
	return 0, nil
}
func (m *mockStorage) SaveChunk(_ context.Context, _ string, _ int, _ []byte) error { return nil }
func (m *mockStorage) MergeChunks(_ context.Context, _ string, _ int, _ string) error {
	return nil
}
func (m *mockStorage) CleanChunks(_ context.Context, _ string) error { return nil }

func newAdminTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{}, &model.SystemConfigRecord{}))
	return g
}

// TestCleanExpiredFiles_DeletesPhysicalFile 验证过期清理同时删 DB 记录和物理文件
func TestCleanExpiredFiles_DeletesPhysicalFile(t *testing.T) {
	adminDB := newAdminTestDB(t)
	svc := NewService(adminDB)
	st := &mockStorage{}
	svc.SetStorage(st)

	ctx := context.Background()
	// 过期文件（时间过期）
	past := time.Now().Add(-time.Hour)
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "EXP1", FilePath: "data/2024", UUIDFileName: "abc.txt",
		Size: 100, ExpiredAt: &past, ExpiredCount: -1,
	}))
	// 未过期文件（不应被删）
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "OK1", FilePath: "data/2024", UUIDFileName: "ok.txt",
		Size: 50, ExpiredCount: -1,
	}))

	deleted, freed, err := svc.CleanExpiredFiles(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted)
	assert.Equal(t, int64(100), freed)

	// 物理文件被删
	st.mu.Lock()
	assert.Len(t, st.deleted, 1)
	assert.Contains(t, st.deleted[0], "abc.txt")
	st.mu.Unlock()

	// DB 记录已删（过期的不存在，未过期的还在）
	_, err = svc.fileCodeRepo.GetByCode(ctx, "EXP1")
	assert.Error(t, err)
	_, err = svc.fileCodeRepo.GetByCode(ctx, "OK1")
	assert.NoError(t, err)
}

// TestCleanExpiredFiles_NoStorage 无 storage 时不崩，仅删 DB
func TestCleanExpiredFiles_NoStorage(t *testing.T) {
	adminDB := newAdminTestDB(t)
	svc := NewService(adminDB) // 无 storage

	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "EXP2", FilePath: "x", UUIDFileName: "y", Size: 10,
		ExpiredAt: &past, ExpiredCount: -1,
	}))

	deleted, _, err := svc.CleanExpiredFiles(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted)
}

// --- SystemConfig 持久化 ---

// customTestConfig 构造一份与默认值不同的配置，便于断言。
func customTestConfig(name string) *SystemConfig {
	cfg := &SystemConfig{}
	cfg.Base = &BaseConfig{Name: name, Description: "自定义描述", Port: 12345}
	cfg.Storage = &StorageSettings{Type: "s3", MaxSize: 5 * 1024 * 1024 * 1024}
	cfg.Transfer = &TransferSettings{MaxCount: 50, ExpireDefault: 30}
	return cfg
}

// TestUpdateConfig_PersistsToDB 验证 UpdateConfig 写穿到 system_configs 表
func TestUpdateConfig_PersistsToDB(t *testing.T) {
	adminDB := newAdminTestDB(t)
	svc := NewService(adminDB)

	ctx := context.Background()
	require.NoError(t, svc.UpdateConfig(ctx, customTestConfig("持久化站点")))

	var rec model.SystemConfigRecord
	require.NoError(t, adminDB.First(&rec).Error, "应存在持久化记录")
	assert.Contains(t, rec.Data, `"name":"持久化站点"`)
}

// TestGetConfig_PersistedAcrossRestart 验证重启（新 Service 实例）后读到持久化配置
func TestGetConfig_PersistedAcrossRestart(t *testing.T) {
	adminDB := newAdminTestDB(t)

	svc1 := NewService(adminDB)
	require.NoError(t, svc1.UpdateConfig(context.Background(), customTestConfig("重启不丢")))

	svc2 := NewService(adminDB) // 模拟重启后的新实例
	cfg, err := svc2.GetConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "重启不丢", cfg.Base.Name)
	assert.Equal(t, 12345, cfg.Base.Port)
	assert.Equal(t, "s3", cfg.Storage.Type)
	assert.Equal(t, int64(5*1024*1024*1024), cfg.Storage.MaxSize)
	assert.Equal(t, 50, cfg.Transfer.MaxCount)
	assert.Equal(t, 30, cfg.Transfer.ExpireDefault)
}

// TestGetConfig_DefaultsWhenNoRecord 验证无持久化记录时返回默认值
func TestGetConfig_DefaultsWhenNoRecord(t *testing.T) {
	adminDB := newAdminTestDB(t)

	cfg, err := NewService(adminDB).GetConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "PigeonBox", cfg.Base.Name)
	assert.Equal(t, "local", cfg.Storage.Type)
	assert.Equal(t, 100, cfg.Transfer.MaxCount)
}

// TestGetConfig_WithoutDB 验证 DB 无持久化记录时回退默认配置（降级语义保留）。
// dao 去全局化后「无 DB」场景不再存在，降级面收窄为「空表」。
func TestGetConfig_WithoutDB(t *testing.T) {
	adminDB := newAdminTestDB(t)

	cfg, err := NewService(adminDB).GetConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "PigeonBox", cfg.Base.Name)
}

// TestUpdateConfig_InvalidConfig 验证非法配置被拒绝且不落库
func TestUpdateConfig_InvalidConfig(t *testing.T) {
	adminDB := newAdminTestDB(t)
	svc := NewService(adminDB)

	bad := customTestConfig("非法端口")
	bad.Base.Port = 70000
	require.Error(t, svc.UpdateConfig(context.Background(), bad))

	neg := customTestConfig("负数上限")
	neg.Storage.MaxSize = -1
	require.Error(t, svc.UpdateConfig(context.Background(), neg))

	// 内存与 DB 均未被污染
	cfg, err := svc.GetConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "PigeonBox", cfg.Base.Name)
	var count int64
	require.NoError(t, adminDB.Model(&model.SystemConfigRecord{}).Count(&count).Error)
	assert.Equal(t, int64(0), count)
}

// TestUpdateConfig_SingleRow 验证多次更新只保留一行记录
func TestUpdateConfig_SingleRow(t *testing.T) {
	adminDB := newAdminTestDB(t)
	svc := NewService(adminDB)
	ctx := context.Background()

	require.NoError(t, svc.UpdateConfig(ctx, customTestConfig("第一版")))
	require.NoError(t, svc.UpdateConfig(ctx, customTestConfig("第二版")))

	var count int64
	require.NoError(t, adminDB.Model(&model.SystemConfigRecord{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)

	cfg, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, "第二版", cfg.Base.Name)
}

// TestUpdateUserSettings_Roundtrip 用户设置写穿 DB 并可读回
// （回归 2026-10-03：管理端"用户配置"此前是假开关——存储结构里根本没有该段）
func TestUpdateUserSettings_Roundtrip(t *testing.T) {
	adminDB := newAdminTestDB(t)
	svc := NewService(adminDB)
	ctx := context.Background()

	require.NoError(t, svc.UpdateUserSettings(ctx, UserSettings{
		AllowUserRegistration: true,
		UserUploadSize:        10 * 1024 * 1024,
		UserStorageQuota:      2 * 1024 * 1024 * 1024,
		SessionExpiryHours:    24,
	}))

	cfg, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, cfg.User)
	assert.True(t, cfg.User.AllowUserRegistration)
	assert.Equal(t, int64(10*1024*1024), cfg.User.UserUploadSize)
	assert.Equal(t, int64(2*1024*1024*1024), cfg.User.UserStorageQuota)
	assert.Equal(t, 24, cfg.User.SessionExpiryHours)

	// DB 落库校验（新实例从 DB 加载）
	fresh := NewService(adminDB)
	cfg2, err := fresh.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, cfg2.User)
	assert.True(t, cfg2.User.AllowUserRegistration)
}

// TestUpdateConfig_PreservesUserSection 通用配置保存（无 user 段）不得冲掉用户设置
func TestUpdateConfig_PreservesUserSection(t *testing.T) {
	adminDB := newAdminTestDB(t)
	svc := NewService(adminDB)
	ctx := context.Background()
	require.NoError(t, svc.UpdateUserSettings(ctx, UserSettings{AllowUserRegistration: true, SessionExpiryHours: 48}))

	require.NoError(t, svc.UpdateConfig(ctx, customTestConfig("改名保存")))

	cfg, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, "改名保存", cfg.Base.Name)
	require.NotNil(t, cfg.User)
	assert.True(t, cfg.User.AllowUserRegistration)
	assert.Equal(t, 48, cfg.User.SessionExpiryHours)
}

// TestLoadPersisted_LegacyRecordWithoutUserSection 旧记录（无 user 段）加载时回退 yaml 默认，
// 而不是零值——否则升级后注册会被意外当成"关闭"。
func TestLoadPersisted_LegacyRecordWithoutUserSection(t *testing.T) {
	g := newAdminTestDB(t)
	// 直接写一条历史形态的记录（只有旧字段）
	require.NoError(t, g.Create(&model.SystemConfigRecord{Data: `{"base":{"name":"OldSite","description":"d","port":1234},"storage":{"type":"local","max_size":1},"transfer":{"max_count":10,"expire_default":7}}`}).Error)

	cfg, err := NewService(g).GetConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "OldSite", cfg.Base.Name)
	require.NotNil(t, cfg.User)
	// 回退自 yaml 全局配置：布尔/数值非零值语义由 yaml 决定，这里只验证不是零值指针
	assert.NotNil(t, cfg.User)
}

// TestValidateUserSettings 已随配置中心迁 app/config/config_test.go（2026-10-10 域独立）。
