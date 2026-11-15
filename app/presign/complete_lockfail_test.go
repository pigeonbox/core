// core#183：完成面 fail-closed——抢锁失败（ctx 取消/超时）时 Complete 必须在
// 触碰任何状态之前直接返回错误，不读 meta、不建分享。
//
// 缺陷背景（D02/#158 残差）：uploadlock.Lock 的 ctx 失败路径此前无用例，
// 「拿不到锁就什么都不做」的 fail-closed 语义没被钉住——若有人把 presign.go
// Complete 里的 lockErr 短路误删，锁失败会落回 read-then-create 的并发窗口
// （同一 upload_id 建两条分享、回两个取件码）。
//
// 负对照：删掉该 lockErr 短路 ⇒ 本用例在「锁被他人持有」时会继续执行
// （排队挂死或进入建分享）⇒ 转红。
package presign

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/internal/uploadlock"
)

func TestComplete_LockFailClosed(t *testing.T) {
	svc, _ := newTestService(t)
	mock := &concurrentShareService{bothIn: make(chan struct{})}
	svc.SetShareService(mock)

	initRes, err := svc.Init(context.Background(), InitMeta{FileName: "lockfail.txt", FileSize: 1024})
	require.NoError(t, err)

	// 他人持有该 uploadID 的完成面锁（进程内全局 KeyedGate，cap=1）
	release, err := uploadlock.Lock(context.Background(), initRes.UploadID)
	require.NoError(t, err)
	defer release()

	// ctx 已取消 + 槽被占 ⇒ Lock 确定返回 context.Canceled（select 只此一选）
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := svc.Complete(canceled, initRes.UploadID, initRes.Token, "1.2.3.4")
	require.Error(t, err, "抢锁失败时 Complete 必须返回错误（fail-closed）")
	require.Nil(t, res, "抢锁失败时不得返回结果")
	require.Zero(t, mock.calls, "抢锁失败时不得进入建分享（否则并发窗口复活）")
}
