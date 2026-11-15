// 配额 check-then-act 的并发打穿（core#169）
//
// 缺陷：配额「读合计 → 判定 →（调用方之后）insert」无事务无锁——
// N 个并发 CreateShare 各自读到同一 used、全部放行后各自 insert ⇒ 落库合计
// 可到 quota + (N-1)×size。站点级（storage.quota）与用户级（CheckQuota）
// 两条链同病。
//
// 负对照（issue 指定口径）：quota=100、并发各 20 ⇒ 断言落库合计 ≤100。
// 种子 80 只留 20 空位：缺陷态下并发全部通过判定即打穿（合计 80+N×20）。
//
// 断言必须能变红：抽掉预留账本（reserveUploadCaps 退回裸读-比-放行）⇒ 两用例红。
package share

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	userapp "github.com/pigeonbox/core/app/user"
	"github.com/pigeonbox/core/repo/db/model"
)

// runConcurrentShares n 个并发 CreateShare（同规格 Size），返回成功数。
func runConcurrentShares(t *testing.T, svc *Service, userID *uint, n int, each int64) int {
	t.Helper()
	ctx := context.Background()
	expired := time.Now().Add(24 * time.Hour)
	var wg sync.WaitGroup
	start := make(chan struct{})
	ok := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := svc.CreateShare(ctx, &ShareFileReq{
				Channel:      "direct",
				FilePath:     fmt.Sprintf("uploads/conc-%d.bin", i),
				Size:         each,
				Text:         fmt.Sprintf("conc-%d.bin", i),
				ExpiredAt:    &expired,
				ExpiredCount: -1,
				UploadType:   "authenticated",
				UserID:       userID,
			})
			ok[i] = err == nil
		}(i)
	}
	close(start)
	wg.Wait()
	passed := 0
	for _, o := range ok {
		if o {
			passed++
		}
	}
	return passed
}

// TestUploadCaps_ConcurrentSiteQuotaAtomic 站点级：并发不得打穿 storage.quota。
func TestUploadCaps_ConcurrentSiteQuotaAtomic(t *testing.T) {
	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	setStorageQuota(t, 100)
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "CCSEED1", FilePath: "uploads/seed.bin", Size: 80, Text: "seed.bin",
		ExpiredCount: -1,
	}).Error)

	ok := runConcurrentShares(t, svc, nil, 10, 20)

	var total int64
	require.NoError(t, gormDB.Model(&model.FileCode{}).
		Select("COALESCE(SUM(size), 0)").Scan(&total).Error)
	require.LessOrEqual(t, total, int64(100),
		"并发下落库合计 %d 打穿站点配额 100（check-then-act 非原子）", total)
	require.Equal(t, 1, ok,
		"只剩 20 空位：必须恰有 1 个成功（既不打穿也不是一律拒），实得 %d", ok)

	// 预留账本必须归零：漏释放 ⇒ 后续上传被过度拒绝（保守面缺口）
	require.Zero(t, quotaReserve.site, "站点在途预留必须归零")
}

// TestUploadCaps_ConcurrentUserQuotaAtomic 用户级：并发不得打穿用户存储配额。
// 装配真实 app/user.Service 作 QuotaChecker（生产同构：bootstrap 注入同一实例）。
func TestUploadCaps_ConcurrentUserQuotaAtomic(t *testing.T) {
	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	require.NoError(t, gormDB.AutoMigrate(&model.User{}))
	setStorageQuota(t, 0) // 站点闸关闭，隔离用户闸
	svc.SetQuotaChecker(userapp.NewService(gormDB))

	u := &model.User{
		Username: "concquota", Email: "concquota@test.local",
		Status: "active", Role: "user", MaxStorageQuota: 100,
	}
	require.NoError(t, gormDB.Create(u).Error)
	uid := u.ID
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "CCUSEED", FilePath: "uploads/seed.bin", Size: 80, Text: "seed.bin",
		ExpiredCount: -1, UserID: &uid,
	}).Error)

	ok := runConcurrentShares(t, svc, &uid, 10, 20)

	var total int64
	require.NoError(t, gormDB.Model(&model.FileCode{}).Where("user_id = ?", uid).
		Select("COALESCE(SUM(size), 0)").Scan(&total).Error)
	require.LessOrEqual(t, total, int64(100),
		"并发下用户落库合计 %d 打穿配额 100（CheckQuota 读-比-放行非原子）", total)
	require.Equal(t, 1, ok,
		"只剩 20 空位：必须恰有 1 个成功，实得 %d", ok)

	require.Zero(t, quotaReserve.user[uid], "用户在途预留必须归零")
}
