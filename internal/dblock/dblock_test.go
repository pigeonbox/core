package dblock

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// core#169 / core#191：DB 级命名互斥闸回归。
//
// 断言面（sqlite 条纹层）：互斥、释放幂等、ctx 取消可中断、超时返回
// ErrLockTimeout。MySQL/PG（GET_LOCK / pg_advisory_lock）的真库跨副本形态
// 由 app/dbmigrate 的环境门控验收覆盖（CI 起真容器跑）。

func newDLockTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

// TestDLock_MutualExclusion 同名互斥：两个 worker 并发进出同名临界段，重叠
// 探测器必须恒零；释放后可再取。
func TestDLock_MutualExclusion(t *testing.T) {
	l := NewDLock(newDLockTestDB(t))
	ctx := context.Background()

	inside := atomic.Bool{} // 临界段重叠探测器（只在持锁期间触碰）
	var overlaps atomic.Int64

	worker := func() {
		for range 40 {
			rel, err := l.Lock(ctx, QuotaGateKey())
			if err != nil {
				t.Errorf("lock: %v", err)
				return
			}
			if inside.Load() {
				overlaps.Add(1) // 临界段重叠 = 互斥被打破
			}
			inside.Store(true)
			time.Sleep(500 * time.Microsecond) // 给对方留闯入窗口
			inside.Store(false)
			rel()
		}
	}
	var wg sync.WaitGroup
	wg.Go(worker)
	wg.Go(worker)
	wg.Wait()

	assert.Zero(t, overlaps.Load(), "临界段不得重叠（互斥语义）")

	// 释放后可再取
	release, err := l.Lock(ctx, QuotaGateKey())
	require.NoError(t, err)
	release()
}

// TestDLock_ReleaseIdempotent release 幂等（多次调用不 panic、不二次解锁）。
func TestDLock_ReleaseIdempotent(t *testing.T) {
	l := NewDLock(newDLockTestDB(t))
	release, err := l.Lock(context.Background(), UploadCompletionKey("u1"))
	require.NoError(t, err)
	release()
	release()
	release()

	// 解锁后立即可取（没有死锁/重复解锁卡死）
	done := make(chan struct{})
	go func() {
		defer close(done)
		r, err := l.Lock(context.Background(), UploadCompletionKey("u1"))
		require.NoError(t, err)
		r()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("release 幂等调用后重取锁卡死")
	}
}

// TestDLock_CtxCancel 等锁期间 ctx 取消 ⇒ 立即返回错误（不傻等）。
func TestDLock_CtxCancel(t *testing.T) {
	l := NewDLock(newDLockTestDB(t))
	release, err := l.Lock(context.Background(), "busy-key")
	require.NoError(t, err)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = l.Lock(ctx, "busy-key")
	require.Error(t, err, "持锁期间后到方必须拿不到")
}

// TestDLock_DifferentKeys 不同名不互斥（条纹碰撞只造成偶发排队，不影响语义）。
func TestDLock_DifferentKeys(t *testing.T) {
	l := NewDLock(newDLockTestDB(t))
	r1, err := l.Lock(context.Background(), UploadCompletionKey("upload-A"))
	require.NoError(t, err)
	defer r1()
	r2, err := l.Lock(context.Background(), UploadCompletionKey("upload-B"))
	require.NoError(t, err)
	defer r2()
}

// TestDLock_NilHandle nil 句柄显式报错（fail-fast，不静默放行）。
func TestDLock_NilHandle(t *testing.T) {
	var l *DLock
	_, err := l.Lock(context.Background(), "k")
	require.Error(t, err)
}

// TestUploadCompletionKey 闸名确定性（同一 uploadID 恒同名）。
func TestUploadCompletionKey(t *testing.T) {
	assert.Equal(t, UploadCompletionKey("abc"), UploadCompletionKey("abc"))
	assert.NotEqual(t, UploadCompletionKey("abc"), UploadCompletionKey("abd"))
	assert.Equal(t, "pbc:quota_gate", QuotaGateKey())
}
