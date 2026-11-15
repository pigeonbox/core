// hub#364 强制测试①（L5 并发幂等）：100 个并发 presign Complete 打同一
// upload_id ⇒ 只允许**一条分享记录**（100 方拿到同一个分享码）。
//
// 与既有 2 并发回归（concurrent_complete_test.go，core#151）的区别：并发量拉到
// 100（issue 正文判据）。本文件自带 mock（不引用 concurrent_complete_test.go 的
// helper——#196 在途正改那个文件）。
//
// 竞态窗口钉死（不靠调度运气）：mock 的 CreateShare 等「100 个并发方都到齐」
// 或超时放行——无锁形态下 100 方都读到 Complete=false 必然都数进建分享计数；
// 有锁形态下后到方被挡在 uploadlock 外、到不了 CreateShare，首方超时自行放行。
//
// 正判据：100 方全部成功、分享码全同、CreateShare 恰好被调 1 次。
// 负对照：撤掉 presign Complete 入口的 uploadlock.Lock ⇒ 100 方都进
// CreateShare ⇒ calls≫1 ⇒ 本用例转红（PR 里附实跑结果）。
package presign

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// share100Mock 带「100 方到齐屏障」的 share service 桩。
type share100Mock struct {
	calls    atomic.Int64
	mu       sync.Mutex
	existing string

	arrived chan struct{} // 第 n 方 CreateShare 到达时关闭
	once    sync.Once
	n       int
}

func newShare100Mock(n int) *share100Mock {
	return &share100Mock{arrived: make(chan struct{}), n: n}
}

func (m *share100Mock) CreateShare(ctx context.Context, req *ShareCreateReq) (*ShareCreateResp, error) {
	c := m.calls.Add(1)
	if int(c) >= m.n {
		m.once.Do(func() { close(m.arrived) })
	}
	// 等全部并发方到齐（无锁必达）；有锁时只有首方到、超时放行。
	select {
	case <-m.arrived:
	case <-time.After(1500 * time.Millisecond):
	}

	m.mu.Lock()
	m.existing = "SHARE100"
	m.mu.Unlock()
	return &ShareCreateResp{
		Code:         "SHARE100",
		ShareURL:     "/share/SHARE100",
		FullShareURL: "http://test.local/#/share/SHARE100",
	}, nil
}

func (m *share100Mock) FindShareByUploadIDOwnedBy(ctx context.Context, uploadID string, userID *uint, ownerIP string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.existing, nil
}

// TestComplete_Concurrent100_OnlyOneShare 100 并发 Complete 同一 upload_id：
// 只建一条分享、100 方拿到同一个分享码。
func TestComplete_Concurrent100_OnlyOneShare(t *testing.T) {
	svc, _ := newTestService(t)
	mock := newShare100Mock(100)
	svc.SetShareService(mock)

	ctx := context.Background()
	initRes, err := svc.Init(ctx, InitMeta{FileName: "race100.txt", FileSize: 1024})
	require.NoError(t, err)

	const n = 100
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]*CompleteResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Go(func() {
			<-start
			results[i], errs[i] = svc.Complete(ctx, initRes.UploadID, initRes.Token, "1.2.3.4")
		})
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		require.NoError(t, errs[i], "第 %d 方 Complete 应成功（幂等）", i)
		require.NotNil(t, results[i])
		require.NotEmpty(t, results[i].ShareCode, "第 %d 方应拿到分享码", i)
		assert.Equal(t, results[0].ShareCode, results[i].ShareCode,
			"第 %d 方必须拿到同一个分享码（同属主幂等）", i)
	}
	assert.Equal(t, int64(1), mock.calls.Load(),
		"100 并发 Complete 只能建一条分享（否则取件额度翻 100 倍）")
}
