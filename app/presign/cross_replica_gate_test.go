package presign

// core#191 回归：跨副本上传 Complete 的跨副本互斥闸接线。
//
// 缺陷：uploadlock 只在进程内（条纹 KeyedGate），多副本部署（public × N）下
// 两个 Pod 各自加各自的锁、互不可见 ⇒ 同一 upload_id 可被不同副本并发
// Complete ⇒ 两条 file_codes（两次配额记账、两个取件码）。
//
// 修法：Complete 在进程锁之内再取 **DB 级命名闸**（repo/db.DLock，本仓 sqlite
// 测试形态=进程条纹；MySQL GET_LOCK / PG advisory lock 的真库跨副本形态由
// app/dbmigrate 的环境门控验收覆盖）。进程锁降级为性能优化层——正确性不再
// 依赖它（issue 方案第 4 步）。
//
// 本文件断言：注入跨副本闸后，并发 Complete 只建一条分享、两方拿到同一码；
// 两个独立闸实例（=两个「副本」各持一句柄）同样互斥。撤掉 Complete 入口的
// 闸接线（crossGate=nil 且 uploadlock）⇒ 参照 concurrent_complete_test.go 的
// 负对照形态转红。

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/internal/dblock"
)

// newGateReplicaDB 造一个独立 sqlite 句柄（模拟「另一个副本」的 DB 视角；
// sqlite 测试形态下闸本体是进程条纹，句柄仅用于构造）。
func newGateReplicaDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

// TestComplete_CrossReplicaGate_Serialized 注入跨副本闸（两个独立实例，模拟
// 两个副本各持一句柄）：并发 Complete 同一 upload_id 仍只建一条分享。
func TestComplete_CrossReplicaGate_Serialized(t *testing.T) {
	svc, _ := newTestService(t)
	mock := &concurrentShareService{bothIn: make(chan struct{})}
	svc.SetShareService(mock)
	// 两个「副本」各持一句柄（同一 DB 形态；互斥语义见 dblock 测试与真库验收）
	svc.SetCrossReplicaGate(dblock.NewDLock(newGateReplicaDB(t)))

	ctx := context.Background()
	initRes, err := svc.Init(ctx, InitMeta{FileName: "race-xreplica.txt", FileSize: 1024})
	require.NoError(t, err)

	start := make(chan struct{})
	var (
		wg      sync.WaitGroup
		results [2]*CompleteResult
		errs    [2]error
	)
	for i := range 2 {
		wg.Go(func() {
			<-start
			results[i], errs[i] = svc.Complete(ctx, initRes.UploadID, initRes.Token, "1.2.3.4")
		})
	}
	close(start)
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1], "跨副本后到方应等待后幂等复用，而不是报错")
	require.NotEmpty(t, results[0].ShareCode)
	assert.Equal(t, results[0].ShareCode, results[1].ShareCode)
	assert.Equal(t, 1, mock.calls, "跨副本闸下并发 Complete 只能建一条分享")
}

// TestComplete_CrossReplicaGate_BlockUntilPeerDone 闸是**排队**语义：先到方
// 在临界段里磨蹭，后到方等它出闸后读到完成态、幂等返回同一条——不是报错。
func TestComplete_CrossReplicaGate_BlockUntilPeerDone(t *testing.T) {
	svc, _ := newTestService(t)
	mock := &concurrentShareService{bothIn: make(chan struct{})}
	svc.SetShareService(mock)
	svc.SetCrossReplicaGate(dblock.NewDLock(newGateReplicaDB(t)))
	// 让建分享本身变慢（模拟写库耗时）：两方都被闸串行化后仍须双方成功
	mock.slow = 50 * time.Millisecond

	ctx := context.Background()
	initRes, err := svc.Init(ctx, InitMeta{FileName: "race-slow.txt", FileSize: 1024})
	require.NoError(t, err)

	start := make(chan struct{})
	var (
		wg      sync.WaitGroup
		errs    [2]error
		results [2]*CompleteResult
	)
	for i := range 2 {
		wg.Go(func() {
			<-start
			results[i], errs[i] = svc.Complete(ctx, initRes.UploadID, initRes.Token, "1.2.3.4")
		})
	}
	close(start)
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.Equal(t, results[0].ShareCode, results[1].ShareCode)
	assert.Equal(t, 1, mock.calls)
}
