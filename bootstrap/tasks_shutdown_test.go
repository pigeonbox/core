package bootstrap

// 后台任务生命周期验证：teardown 取消 → 排水语义（logx/shutdown 接线的消费契约）。

import (
	"context"
	"testing"
	"time"
)

func TestBackgroundTasks_DrainAfterCancel(t *testing.T) {
	root := backgroundContext()
	// 模拟在飞轮次：取消后收尾前有 50ms 收尾工作
	bgWg.Add(1)
	go func() {
		<-root.Done()
		time.Sleep(50 * time.Millisecond)
		bgWg.Done()
	}()

	stopBackgroundTasks()
	if err := drainBackgroundTasks(context.Background()); err != nil {
		t.Fatalf("取消后排水应等在飞轮次收尾并返回 nil: %v", err)
	}
}

func TestBackgroundTasks_DrainTimeout(t *testing.T) {
	// 未收尾的轮次：排水按 ctx 超时预算报错（在飞工作被放弃，交进程退出收尾）
	bgWg.Add(1)
	defer bgWg.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := drainBackgroundTasks(ctx); err == nil {
		t.Fatal("在飞未收尾时排水应按预算报超时")
	}
}
