package uploadlock

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ctx 取消/超时导致抢锁失败时必须 fail-closed（core#183 残差 D02/#158）：
// 返回 ctx.Err() 且**不发放 release**——调用方（presign/chunk 完成面）拿到 err
// 就直接返回，绝不能带着一个无效 release 去「释放」别人持有的锁。
//
// 构造确定性（不靠调度运气）：KeyedGate.Acquire 是 select{ ch<- / <-ctx.Done() }，
// 若槽空闲且 ctx 已取消，两个 case 同时 ready ⇒ 随机命中，测试会 flaky。故先
// 占满同 key 的槽（cap=1），让 ch<- 恒阻塞 ⇒ 只剩 ctx.Done() 可选。
//
// 负对照：让 Lock 忽略 ctx（或 Acquire 不看 ctx.Done）⇒ 下面断言转红。
func TestLock_CanceledCtxFailsClosed(t *testing.T) {
	ctx := context.Background()
	release, err := Lock(ctx, "up-ctxfail")
	if err != nil {
		t.Fatalf("首次 Lock: %v", err)
	}
	defer release()

	canceled, cancel := context.WithCancel(context.Background())
	cancel() // 抢锁前已取消
	got, err := Lock(canceled, "up-ctxfail")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("槽被占 + ctx 已取消 ⇒ 必须返回 context.Canceled, got %v", err)
	}
	if got != nil {
		t.Fatal("抢锁失败不得发放 release（否则调用方会误释放他人持有的锁）")
	}
}

// 排队等待中取消：等待方以 ctx.Err() 退出，且**不占名额**——失败的抢锁没有
// 泄漏槽位，否则 cap=1 的槽被占死、该 uploadID 的完成面永久卡死。
//
// 时序不敏感：无论 cancel() 先于还是后于排队方进入 select，首方都占着槽
// ⇒ ch<- 恒阻塞 ⇒ 只有 ctx.Done() 可选 ⇒ 恒返回 Canceled（本用例在收到
// errCh 之前不会 release，故排队方看到的槽始终是满的）。
func TestLock_QueueWaitCancel_NoSlotLeak(t *testing.T) {
	ctx := context.Background()
	release, err := Lock(ctx, "up-queue")
	if err != nil {
		t.Fatalf("首次 Lock: %v", err)
	}

	waitCtx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		rel, werr := Lock(waitCtx, "up-queue")
		if rel != nil {
			rel() // 不应发生；即便发生也不留名额
		}
		errCh <- werr
	}()

	cancel()
	select {
	case werr := <-errCh:
		if !errors.Is(werr, context.Canceled) {
			t.Fatalf("排队中取消 ⇒ 必须返回 context.Canceled, got %v", werr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("排队中的 Lock 应在 ctx 取消后返回（否则完成面会挂死）")
	}

	// 名额未被失败的抢锁占走：原持有者释放后，新的正常抢锁必须成功
	release()
	rel, err := Lock(ctx, "up-queue")
	if err != nil {
		t.Fatalf("释放后正常 Lock 应成功（失败的抢锁泄漏了名额）: %v", err)
	}
	rel()
}
