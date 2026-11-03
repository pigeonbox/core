// 会话存活期 ≠ 签名时效：慢速大文件「传完即会话消失」守卫（2026-11-03 真机缺陷驱动）。
//
// 缺陷实录（10.10.10.3，trace 3a3273a1）：同事传 2.5GB istoreos .img，
// PUT 实测 1717 秒（28.6 分钟）返回 200，27ms 后 presign/complete 却报
// 10004「not found」，2.5GB 对象成孤儿。根因三连：
//
//	① 会话时效 download.presign_expire_seconds（线上=600s）被当成**传输时效**，
//	   meta 键在传输途中被 Redis TTL 静默清掉；
//	② PUT 只在开头查过期 ⇒ 流越过 ExpireAt 照常传完并返回 200（数据面成功、
//	   控制面已死）；
//	③ PUT 结束按 `remaining=Until(ExpireAt)` 且仅在 >0 时回写 meta ⇒ 传完时
//	   ExpireAt 早已过去 ⇒ 回写被整个跳过（Transferred/FileHash 全丢）。
//
// 语义修正：签名时效只约束「PUT 发起时刻」；PUT 结束**无条件**回写 meta 并带
// Complete 宽限（键重建）；数据已传完（Transferred）的会话 Complete 不再被
// ExpireAt 拒绝；s3 方案 Init 即按 transferWindow 存 meta、过期放行以对象存在
// 性核实为等价判据。
//
// ⚠️ 判据量是墙钟：签名时效过没过期由 time.Time 判定，miniredis 快进只假造键
// TTL、骗不过它 ⇒ Service.nowFn 注入 fakeClock，传输中同时推两个时钟。
//
// 断言必须能变红（负对照，均已实测）：
//   - 整体回退旧实现 ⇒ TestComplete_AfterSlowUpload_Succeeds 红，错误与线上
//     wire 一致（ErrUploadNotFound/10004 not found）；
//   - PUT 结束回写改回 `未过期才写` ⇒ 同用例红（Transferred 未持久化）；
//   - Complete 的 Transferred/S3 过期放行改回无条件过期拒绝 ⇒ 同用例红
//     （ErrUploadExpired）；
//   - 去掉 Init 的 s3 metaTTL 放宽 ⇒ TestComplete_S3LongUpload_SessionSurvives 红。
package presign

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock 可推进的测试时钟（Service.nowFn 的时间源）。
type fakeClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// slowReader 模拟「传输期间墙钟流逝」：首读时同时推进 fakeClock 与 miniredis
// 键 TTL，等价于真机 28.6 分钟的传输时长在 PUT **进行中**流逝（而非传完后再流逝）。
type slowReader struct {
	r     *bytes.Reader
	mr    *miniredis.Miniredis
	clock *fakeClock
	fast  time.Duration
	fired bool
}

func (s *slowReader) Read(p []byte) (int, error) {
	if !s.fired {
		s.fired = true
		s.clock.Advance(s.fast)
		s.mr.FastForward(s.fast)
	}
	return s.r.Read(p)
}

// TestComplete_AfterSlowUpload_Succeeds 真机场景正片：会话时效 600s、PUT 发起后
// 远超 ExpireAt 才传完 ⇒ Complete 必须成功建分享（不得报 not found / expired）。
func TestComplete_AfterSlowUpload_Succeeds(t *testing.T) {
	svc, mr := newTestService(t)
	clock := &fakeClock{}
	svc.nowFn = clock.Now
	st := newFakeStorage()
	svc.SetStorage(st)
	mock := &mockShareService{returnCode: "SHARE-IMG"}
	svc.SetShareService(mock)

	ctx := context.Background()
	body := bytes.Repeat([]byte{0x5A}, 4096)
	initRes, err := svc.Init(ctx, InitMeta{
		FileName:    "istoreos-25.12.6-x86-64-squashfs-combined-efi.img",
		FileSize:    int64(len(body)),
		SessionTTL:  600 * time.Second, // 线上 download.presign_expire_seconds=600
		ExpireValue: 1,
		ExpireStyle: "day",
	})
	require.NoError(t, err)

	// PUT 发起（此刻会话有效），传输中推进 40 分钟（真机 1717s）再传完
	require.NoError(t, svc.UploadDirect(ctx, initRes.UploadID, initRes.Token,
		&slowReader{r: bytes.NewReader(body), mr: mr, clock: clock, fast: 40 * time.Minute}, int64(len(body))))

	res, err := svc.Complete(ctx, initRes.UploadID, initRes.Token, "111.201.208.122")
	require.NoError(t, err,
		"数据已传完的会话不得因签名时效拒绝 Complete（旧实现：ErrUploadNotFound/ErrUploadExpired，2.5GB 对象成孤儿）")
	assert.Equal(t, "SHARE-IMG", res.ShareCode)
	assert.Equal(t, 1, mock.called)
}

// TestComplete_NoTransfer_AfterExpiry_Rejected 负面对照：未传数据的过期会话
// 仍然必须被拒（放行只给「数据面已完成」的会话）⇒ 语义放宽不得开洞。
func TestComplete_NoTransfer_AfterExpiry_Rejected(t *testing.T) {
	svc, mr := newTestService(t)
	clock := &fakeClock{}
	svc.nowFn = clock.Now
	st := newFakeStorage()
	svc.SetStorage(st)
	mock := &mockShareService{returnCode: "code"}
	svc.SetShareService(mock)

	ctx := context.Background()
	initRes, err := svc.Init(ctx, InitMeta{
		FileName:   "a.img",
		FileSize:   100,
		SessionTTL: 600 * time.Second,
	})
	require.NoError(t, err)

	// 没有 PUT，时钟与键 TTL 一起过期后 Complete
	clock.Advance(20 * time.Minute)
	mr.FastForward(20 * time.Minute)

	_, err = svc.Complete(ctx, initRes.UploadID, initRes.Token, "127.0.0.1")
	require.Error(t, err, "未传数据的过期会话不得 Complete")
	assert.True(t, errors.Is(err, ErrUploadNotFound) || errors.Is(err, ErrUploadExpired),
		"应报会话不存在/已过期，实际: %v", err)
	assert.Equal(t, 0, mock.called, "不得建分享")
}

// TestComplete_S3LongUpload_SessionSurvives s3 直传：传输不经本进程，meta 在
// Init 就须按 transferWindow 存活；ExpireAt 过后以对象存在性核实为准（3.5）。
// 负对照：去掉 Init 的 s3 metaTTL 放宽 ⇒ meta 被 TTL 清掉 ⇒ 红；
//
//	去掉 Complete 的 s3 过期放行 ⇒ ErrUploadExpired ⇒ 红。
func TestComplete_S3LongUpload_SessionSurvives(t *testing.T) {
	svc, mr := newTestService(t)
	clock := &fakeClock{}
	svc.nowFn = clock.Now
	fo := &fakeObjectStore{putURL: "https://s3.example/bucket/", headObj: map[string]int64{}}
	svc.SetObjectStore(fo)
	mock := &mockShareService{returnCode: "SHARE-S3"}
	svc.SetShareService(mock)

	ctx := context.Background()
	initRes, err := svc.Init(ctx, InitMeta{
		FileName:   "big.img",
		FileSize:   4096,
		SessionTTL: 600 * time.Second,
	})
	require.NoError(t, err)
	require.Equal(t, SchemeS3, initRes.Scheme)

	// 客户端已 PUT 到 S3（服务器不见流量），对象真实存在
	fo.headObj[initRes.ObjectKey] = 4096

	// 慢速传输 + 收尾，越过签名时效与键 TTL
	clock.Advance(40 * time.Minute)
	mr.FastForward(40 * time.Minute)

	res, err := svc.Complete(ctx, initRes.UploadID, initRes.Token, "127.0.0.1")
	require.NoError(t, err, "s3 慢速直传后 Complete 不得因签名时效失败")
	assert.Equal(t, "SHARE-S3", res.ShareCode)
	assert.Equal(t, 1, mock.called)
}
