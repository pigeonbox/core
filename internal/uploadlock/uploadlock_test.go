package uploadlock

import (
	"context"
	"testing"
	"time"
)

// 同键排队互斥、异键并行：core#151 依赖的两条基本语义。
//
// 同键判据的负对照：把 gate 换成不互斥的实现（如每次直通）⇒ 首个分支当场红。
// 异键判据必须用**确定不同槽**的键（同槽偶发排队是条纹化的正常代价，不能拿
// 运气当判据）——fnv32 是确定性哈希，这里现场挑键。
func TestLock_SameKeySerializes_DifferentKeysParallel(t *testing.T) {
	ctx := context.Background()

	// ---- 同键：前一个未释放时，后一个必须拿不到 ----
	rel1, err := Lock(ctx, "up-same")
	if err != nil {
		t.Fatalf("首次 Lock: %v", err)
	}
	acquired := make(chan struct{})
	go func() {
		rel2, lerr := Lock(ctx, "up-same")
		if lerr != nil {
			return
		}
		close(acquired)
		rel2()
	}()
	select {
	case <-acquired:
		t.Fatal("同键在前一个未释放时就被拿到（互斥失效）")
	case <-time.After(100 * time.Millisecond):
	}
	rel1()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("前一个释放后，排队方应拿到锁")
	}

	// ---- 异键：不同 uploadID 不得互相挡住 ----
	keyA, keyB := "key-a", "key-b"
	for stripeKey(keyA) == stripeKey(keyB) {
		keyB += "-x" // 挑一个确定落在不同槽的键（条纹碰撞会让本判据失去意义）
	}
	relA, err := Lock(ctx, keyA)
	if err != nil {
		t.Fatalf("Lock(%s): %v", keyA, err)
	}
	relB := make(chan func(), 1)
	go func() {
		r, lerr := Lock(ctx, keyB)
		if lerr != nil {
			close(relB)
			return
		}
		relB <- r
	}()
	select {
	case r := <-relB:
		if r == nil {
			t.Fatal("异键 Lock 失败")
		}
		r()
	case <-time.After(2 * time.Second):
		t.Fatal("异键不应被同槽以外的持锁方挡住（并行性失效）")
	}
	relA()
}
