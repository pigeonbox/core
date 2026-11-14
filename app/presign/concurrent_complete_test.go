// core#151 并发 Complete 竞态回归：同一 upload_id 并发完成只能建**一条**分享。
//
// 缺陷：Complete 的「读 meta.Complete → 建分享 → 标记完成」是 read-then-create
// 且无锁 ⇒ 并发两次（客户端超时重试 / 双击 / HTTP 层重放）各自读到
// Complete=false ⇒ 同一 upload_id 建出 2 条分享、回 2 个取件码 ⇒ 幂等形同虚设。
//
// 正判据：两 goroutine 并发 Complete 同一 upload_id ⇒ 只建 1 条分享、两方拿到
// 同一个分享码（同属主重复 Complete 仍幂等返回同一条）。
// 负对照：撤掉 Complete 入口的 uploadlock.Lock ⇒ 两个 goroutine 都进
// CreateShare ⇒ calls=2 ⇒ 本用例转红。
//
// 竞态窗口钉死（不靠调度运气）：CreateShare 里等「两个并发方都到齐」——无锁
// 形态下两方必然都数进建分享计数；有锁形态下后到方被挡在 uploadlock 外、根本
// 到不了 CreateShare，首个建分享方 500ms 超时自行放行。
package presign

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// concurrentShareService 带「到齐屏障」的 share service 桩：建分享前等两个
// 并发方都到达（无锁必达；有锁时后到方到不了，超时放行），建成后置回读命中
// （模拟真实 share 域「建完即可按 upload_id 回读」）。
type concurrentShareService struct {
	mu       sync.Mutex
	calls    int
	existing string

	bothIn chan struct{} // 第二个 CreateShare 到达时关闭
}

func (m *concurrentShareService) CreateShare(ctx context.Context, req *ShareCreateReq) (*ShareCreateResp, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.mu.Unlock()

	if n == 2 {
		close(m.bothIn)
	}
	select {
	case <-m.bothIn:
	case <-time.After(500 * time.Millisecond):
	}

	m.mu.Lock()
	m.existing = "SHARE001"
	m.mu.Unlock()
	return &ShareCreateResp{
		Code:         "SHARE001",
		ShareURL:     "/share/SHARE001",
		FullShareURL: "http://test.local/#/share/SHARE001",
	}, nil
}

func (m *concurrentShareService) FindShareByUploadIDOwnedBy(ctx context.Context, uploadID string, userID *uint, ownerIP string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.existing, nil
}

// TestComplete_Concurrent_OnlyOneShare 并发 Complete 同一 upload_id：
// 只建一条分享、两方拿到同一个分享码。
func TestComplete_Concurrent_OnlyOneShare(t *testing.T) {
	svc, _ := newTestService(t)
	mock := &concurrentShareService{bothIn: make(chan struct{})}
	svc.SetShareService(mock)

	ctx := context.Background()
	initRes, err := svc.Init(ctx, InitMeta{FileName: "race.txt", FileSize: 1024})
	require.NoError(t, err)

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]*CompleteResult, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() {
			<-start
			results[i], errs[i] = svc.Complete(ctx, initRes.UploadID, initRes.Token, "1.2.3.4")
		})
	}
	close(start)
	wg.Wait()

	require.NoError(t, errs[0], "首个 Complete 应成功")
	require.NoError(t, errs[1], "并发后到方应幂等成功（拿到同一条分享），而不是报错")
	require.NotEmpty(t, results[0].ShareCode)
	assert.Equal(t, results[0].ShareCode, results[1].ShareCode, "并发两方必须拿到同一个分享码（同属主幂等）")
	assert.Equal(t, 1, mock.calls, "并发 Complete 只能建一条分享（否则取件额度翻倍）")
}
