package share

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
)

// setStorageQuota 注入 storage.quota（0=不限），测试结束恢复全局配置——
// TestGlobalStorageQuota 的既有注入方式，抽出供两处用例共用。
func setStorageQuota(t *testing.T, q int64) {
	t.Helper()
	old := conf.GetGlobalConfig()
	var cfg conf.AppConfiguration
	if old != nil {
		cfg = *old
	}
	cfg.Storage.Quota = q
	conf.SetGlobalConfig(&cfg)
	t.Cleanup(func() { conf.SetGlobalConfig(old) })
}

// seedAndSoftDeleteShare 播一条分享并软删进回收站（purge 前仍占盘）。
func seedAndSoftDeleteShare(t *testing.T, gormDB *gorm.DB, code string, size int64) {
	t.Helper()
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: code, FilePath: "uploads/" + code + ".bin", Size: size, Text: code,
		ExpiredCount: -1,
	}).Error)
	require.NoError(t, gormDB.Delete(&model.FileCode{}, "code = ?", code).Error)
}

// 回归：站点级全局存储配额（storage.quota）——全通道闸口 checkUploadCaps，
// 统计口径=**物理占用**（含回收站软删——purge 前仍占盘，评审 D01），quota=0 不限。
func TestGlobalStorageQuota(t *testing.T) {
	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()

	seed := func(code, path string, size int64) {
		require.NoError(t, gormDB.Create(&model.FileCode{
			Code: code, FilePath: path, Size: size, Text: code,
			ExpiredCount: -1,
		}).Error)
	}

	// 存量 800，配额 1000
	seed("GQ0001", "uploads/a.bin", 500)
	seed("GQ0002", "uploads/b.bin", 300)
	setStorageQuota(t, 1000)

	newShare := func(size int64) error {
		_, err := svc.CreateShare(ctx, &ShareFileReq{
			Channel: "direct", FilePath: "uploads/new.bin", Size: size,
			Text: "new.bin",
			ExpiredAt: func() *time.Time {
				t := time.Now().Add(24 * time.Hour)
				return &t
			}(),
			ExpiredCount: -1,
			UploadType:   "authenticated",
		})
		return err
	}

	// 300 → 800+300=1100 > 1000，拒绝
	err := newShare(300)
	require.Error(t, err)
	var gq *GlobalQuotaExceededError
	require.True(t, errors.As(err, &gq), "应为 GlobalQuotaExceededError，实际: %v", err)
	require.Equal(t, int64(800), gq.Used)

	// 100 → 900 ≤ 1000，放行
	require.NoError(t, newShare(100))

	// 物理口径（评审 D01）：软删一条 500 仍占盘 ⇒ 用量 900 不回退，
	// 300 仍超限被拒（修前存活口径 400+300=700 会放行——软删不释放额度）
	require.NoError(t, gormDB.Delete(&model.FileCode{}, "code = ?", "GQ0001").Error)
	err = newShare(300)
	require.Error(t, err, "回收站软删仍占盘，不得释放额度")
	var gq2 *GlobalQuotaExceededError
	require.True(t, errors.As(err, &gq2), "应为 GlobalQuotaExceededError，实际: %v", err)
	require.Equal(t, int64(900), gq2.Used, "物理口径必须含回收站软删")
	// 100 → 900+100=1000 ≤ 1000，恰好放行
	require.NoError(t, newShare(100))

	// quota=0 → 不限
	setStorageQuota(t, 0)
	require.NoError(t, newShare(100000))
}

// 站点配额按物理口径（评审 D01）：软删行占着盘就还占额度——
// 存活=0 但软删=900、quota=1000 时，新传 200 必须被拒。
func TestGlobalQuota_IncludesRecycleBin(t *testing.T) {
	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()
	setStorageQuota(t, 1000)
	seedAndSoftDeleteShare(t, gormDB, "RQ00001", 900) // 回收站里 900

	err := svc.checkUploadCaps(ctx, nil, 200)
	if err == nil {
		t.Fatal("物理口径下必须超限拒绝（回收站软删仍占盘）")
	}
	var gq *GlobalQuotaExceededError
	require.True(t, errors.As(err, &gq), "应为 GlobalQuotaExceededError，实际: %v", err)
	require.Equal(t, int64(900), gq.Used, "物理口径必须把回收站软删算进用量")
}
