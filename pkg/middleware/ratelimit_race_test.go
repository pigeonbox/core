package middleware

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestRateLimiter_UpdateConfigConcurrentWithAllow 是 2026-11-06 配置热更竞态的
// 回归守卫。
//
// 缺陷形态：cfg 只在 UpdateConfig 里持写锁替换，而 allow / addBlock / allowRedis
// 此前**无锁读** rl.cfg，isBlocked / addBlock 也无锁读 rl.rdb ⇒ 与配置热更或
// 装配期 SetRedis 并发时是 data race（Go 内存模型下未定义行为），且可能读到
// 「cfg 已是新值、limiters 还是旧桶」的撕裂态。
//
// ⚠️ 本用例的判据在 `go test -race`：不开 -race 时它只验「并发下不 panic/不死锁」，
// 开了 -race 才会把无锁读写抓成 DATA RACE（回退修复后此处必红，已做负对照）。
func TestRateLimiter_UpdateConfigConcurrentWithAllow(t *testing.T) {
	base := DefaultRateLimitConfig()
	base.Enabled = true
	base.UseRedis = false
	rl := NewRateLimiter(base)
	defer rl.gcCancel()

	// 另一份配置：qps/burst 都不同 ⇒ 触发器量重建，才能暴露「cfg 新、桶旧」与竞态窗
	alt := base
	alt.UploadQPS = base.UploadQPS + 7
	alt.Burst = base.Burst + 3
	alt.BlockSeconds = base.BlockSeconds + 1

	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 流量侧：并发跑命中热路径的三个入口
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rl.allow(ctx, scopeUpload, "203.0.113.7")
				rl.isBlocked(ctx, scopeUpload, "203.0.113.7")
				_ = rl.SnapshotConfig()
			}
		}()
	}

	// 观测侧：Stats 覆盖 limiters / blocked / cfg / rdb 四类字段
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = rl.Stats()
		}
	}()

	// 配置热更 + 装配期 SetRedis 交替发生
	for i := 0; i < 300; i++ {
		if i%2 == 0 {
			rl.UpdateConfig(alt)
		} else {
			rl.UpdateConfig(base)
		}
		if i%50 == 0 {
			rl.SetRedis(nil) // nil ⇒ 回落内存桶；与上面的读侧同一个竞态面
		}
	}

	close(stop)
	wg.Wait()

	// 收尾断言：并发跑完配置必须落到最后一次 UpdateConfig 的值（重建没丢更新）
	if got := rl.SnapshotConfig().UploadQPS; got != base.UploadQPS {
		t.Fatalf("最后一次 UpdateConfig 是 base，快照却是 %d（热更有丢失）", got)
	}
	_ = time.Now()
}
