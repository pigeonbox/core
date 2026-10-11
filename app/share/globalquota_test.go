package share

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
)

// 回归：站点级全局存储配额（storage.quota）——全通道闸口 checkUploadCaps，
// 统计口径=存活 file_codes 合计（软删除不计），quota=0 不限。
func TestGlobalStorageQuota(t *testing.T) {
	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()

	setQuota := func(q int64) {
		old := conf.GetGlobalConfig()
		var cfg conf.AppConfiguration
		if old != nil {
			cfg = *old
		}
		cfg.Storage.Quota = q
		conf.SetGlobalConfig(&cfg)
		t.Cleanup(func() { conf.SetGlobalConfig(old) })
	}

	seed := func(code, path string, size int64) {
		require.NoError(t, gormDB.Create(&model.FileCode{
			Code: code, FilePath: path, Size: size, Text: code,
			ExpiredCount: -1,
		}).Error)
	}

	// 存量 800，配额 1000
	seed("GQ0001", "uploads/a.bin", 500)
	seed("GQ0002", "uploads/b.bin", 300)
	setQuota(1000)

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

	// 软删除一条 500 → 用量 300，300 也能放行
	require.NoError(t, gormDB.Delete(&model.FileCode{}, "code = ?", "GQ0001").Error)
	require.NoError(t, newShare(300))

	// quota=0 → 不限
	setQuota(0)
	require.NoError(t, newShare(100000))
}
