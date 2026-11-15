package share

// core#169 回归：存储配额的「检查 → 扣减」原子性。
//
// 缺陷：checkUploadCaps（站点）/ CheckQuota（用户）是 check-then-act——
// 「读合计 → 比上限 → 放行」之后插入才发生，N 个并发上传各自读到同一 used、
// 全部通过判定后各自 insert ⇒ 实际占用可达 quota + (N-1)×size。并发打穿
// 不需要统计故障，正常高并发即可。
//
// 修法：插入一律走「配额闸」串行段内的单事务（UnderQuotaGateTx +
// quotaGateGuard：闸内重读合计 → 判定 → 主行 → 子行），后到者读到的合计
// 必然包含先到者的插入。
//
// 负对照（issue 判据）：quota=100、10 个并发各 20 ⇒ 修前 10 条全插（合计
// 200 打穿配额）；修后至多 5 条成功、落库合计 ≤ 100。本用例在撤掉
// quotaGateGuard（或闸段退回裸 Create）时必转红。

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// ptrTime 测试用时间指针。
func ptrTime(t time.Time) *time.Time { return &t }

// upsertShare 并发用例的上传请求（匿名直传形态，无秒传指纹）。
func upsertShare(t *testing.T, svc *Service, i int, size int64) error {
	t.Helper()
	return upsertShareAs(t, svc, nil, fmt.Sprintf("uploads/conc/obj-%d.bin", i), size)
}

func upsertShareAs(t *testing.T, svc *Service, userID *uint, path string, size int64) error {
	t.Helper()
	_, err := svc.CreateShare(context.Background(), &ShareFileReq{
		Channel: "direct",
		// 路径各不相同：排除秒传去重分支，聚焦配额闸
		FilePath:     path,
		Size:         size,
		Text:         "conc.bin",
		ExpiredAt:    ptrTime(time.Now().Add(24 * time.Hour)),
		ExpiredCount: -1,
		UploadType:   "anonymous",
		UserID:       userID,
	})
	return err
}

// TestGlobalQuota_ConcurrentCreateNeverOversubscribes 站点配额：quota=100、
// 10 个并发各 20 ⇒ 落库合计 ≤ 100。
func TestGlobalQuota_ConcurrentCreateNeverOversubscribes(t *testing.T) {
	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	setStorageQuota(t, 100)

	const workers = 10
	var (
		wg      sync.WaitGroup
		success atomic.Int64
	)
	for i := range workers {
		wg.Go(func() {
			if err := upsertShare(t, svc, i, 20); err == nil {
				success.Add(1)
			}
		})
	}
	wg.Wait()

	var total int64
	require.NoError(t, gormDB.Unscoped().Model(&model.FileCode{}).
		Select("COALESCE(SUM(size), 0)").Scan(&total).Error)

	assert.LessOrEqual(t, total, int64(100), "并发上传不得打穿站点配额（修前=200）")
	assert.EqualValues(t, total, success.Load()*20, "成功数与落库合计必须一致（闸段事务内原子落账）")
	assert.LessOrEqual(t, success.Load(), int64(5), "quota=100、单次 20 ⇒ 至多 5 条成功")
}

// fakeQuotaLimitResolver 用户级配额上限桩（闸内权威复核消费）。
type fakeQuotaLimitResolver struct{ limit int64 }

func (f fakeQuotaLimitResolver) GetStorageQuotaLimit(_ context.Context, _ uint) int64 {
	return f.limit
}

// TestUserQuota_ConcurrentCreateNeverOversubscribes 用户配额：上限 100、
// 同一用户 10 个并发各 20 ⇒ 该用户落库合计 ≤ 100。
func TestUserQuota_ConcurrentCreateNeverOversubscribes(t *testing.T) {
	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	setStorageQuota(t, 0) // 站点不限，聚焦用户级闸
	svc.SetQuotaLimitResolver(fakeQuotaLimitResolver{limit: 100})

	const uid = uint(7)
	const workers = 10
	var (
		wg      sync.WaitGroup
		success atomic.Int64
	)
	for i := range workers {
		wg.Go(func() {
			uidCopy := uid
			if err := upsertShareAs(t, svc, &uidCopy, fmt.Sprintf("uploads/conc/user-obj-%d.bin", i), 20); err == nil {
				success.Add(1)
			}
		})
	}
	wg.Wait()

	var used int64
	require.NoError(t, gormDB.Model(&model.FileCode{}).
		Where("user_id = ?", uid).
		Select("COALESCE(SUM(size), 0)").Scan(&used).Error)

	assert.LessOrEqual(t, used, int64(100), "同一用户并发上传不得打穿用户配额（修前=200）")
	assert.LessOrEqual(t, success.Load(), int64(5), "上限 100、单次 20 ⇒ 至多 5 条成功")
}

// TestCreateShare_UserQuotaExceededTypedError 闸内用户级判定返回 typed error
// （handler 侧经 ErrCode 透传 CodeStorageQuota，与预检路径同码）。
func TestCreateShare_UserQuotaExceededTypedError(t *testing.T) {
	svc, _, _, _, _ := newTestServiceWithDB(t)
	setStorageQuota(t, 0)
	svc.SetQuotaLimitResolver(fakeQuotaLimitResolver{limit: 50})

	uid := uint(3)
	err := upsertShareAs(t, svc, &uid, "uploads/conc/guard.bin", 60)
	require.Error(t, err)
	var uq *UserQuotaExceededError
	require.ErrorAs(t, err, &uq, "闸内用户级超限必须是 UserQuotaExceededError: %v", err)
	require.EqualValues(t, 50, uq.Limit)

	// 60 拒后再传 40 ⇒ 通过（合计 40 ≤ 50）
	require.NoError(t, upsertShareAs(t, svc, &uid, "uploads/conc/guard2.bin", 40))
}
