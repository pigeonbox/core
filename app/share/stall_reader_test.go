// stall_reader_test.go —— 下载停滞守卫（download.download_timeout 接线）。
//
// 语义钉住：传输中连续 N 秒无**任何字节流动**即中断下载（停滞无论来自后端存储
// 还是客户端不读）。关键用例：
//
//	慢速持续流不被误杀 —— 每 30ms 一滴、总时长远超 stall，但字节从未断流
//	（本组用例同时是 timer.Reset 续期的变异负对照：去掉 Reset 即转红）
//	真停滞被中断     —— 流挂死（无字节流动）⇒ 读端在 ~stall 内拿到 ErrDownloadStalled
//	客户端停滞可回收  —— 调用方不读 ⇒ 泵阻塞在写端 ⇒ 看门狗到期关底层流
//	<=0 不限          —— 原样返回底层流（不建管道、不起协程）
//	Close 单次关底层  —— 泵退出与调用方 Close 并发也只关一次
package share

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// fakeStream 可编程读流：data 读尽后按 block 决定挂死或 EOF；
// Close 幂等计数（验证"底层只关一次"）。
type fakeStream struct {
	data   []byte
	off    int
	block  bool
	closed chan struct{}
	once   sync.Once
	mu     sync.Mutex
	closes int
}

func newFakeStream(data []byte, block bool) *fakeStream {
	return &fakeStream{data: data, block: block, closed: make(chan struct{})}
}

func (f *fakeStream) Read(p []byte) (int, error) {
	if f.off >= len(f.data) {
		if f.block {
			<-f.closed // 挂死直到被 Close 唤醒
			return 0, errors.New("stream closed")
		}
		return 0, io.EOF
	}
	n := copy(p, f.data[f.off:])
	f.off += n
	return n, nil
}

func (f *fakeStream) Close() error {
	f.mu.Lock()
	f.closes++
	f.mu.Unlock()
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeStream) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

// dripStream 慢速滴流：每 tick 一个字节，共 n 滴（模拟慢速但持续的传输）。
type dripStream struct {
	ch     chan byte
	closed chan struct{}
	once   sync.Once
}

func newDripStream(n int, tick time.Duration) *dripStream {
	d := &dripStream{ch: make(chan byte), closed: make(chan struct{})}
	go func() {
		for i := 0; i < n; i++ {
			time.Sleep(tick)
			d.ch <- 'x'
		}
		close(d.ch)
	}()
	return d
}

func (d *dripStream) Read(p []byte) (int, error) {
	b, ok := <-d.ch
	if !ok {
		return 0, io.EOF
	}
	p[0] = b
	return 1, nil
}

func (d *dripStream) Close() error {
	d.once.Do(func() { close(d.closed) })
	return nil
}

// TestStallGuardPassThrough 正常流全程透传（含 EOF 语义）。
func TestStallGuardPassThrough(t *testing.T) {
	data := bytes.Repeat([]byte("pigeonbox-"), 5000) // 50KB
	rc := newStallGuard(newFakeStream(data, false), time.Second)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("数据不一致: got %d bytes want %d", len(got), len(data))
	}
}

// TestStallGuardSlowButFlowingNotKilled 慢速持续流不被误杀：每 30ms 一滴、
// 总时长 180ms 远超 stall=80ms，但字节从未断流 ⇒ 必须全部读到。
// ⚠ 变异负对照：去掉泵里的 timer.Reset 续期 ⇒ 80ms 处看门狗误杀 ⇒ 本用例转红。
func TestStallGuardSlowButFlowingNotKilled(t *testing.T) {
	const n = 6
	drip := newDripStream(n, 30*time.Millisecond)
	rc := newStallGuard(drip, 80*time.Millisecond)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("慢速持续流被误杀: %v", err)
	}
	if len(got) != n {
		t.Fatalf("字节数不对: got %d want %d", len(got), n)
	}
}

// TestStallGuardStallFires 真停滞（流挂死、无字节流动）⇒ ~stall 内中断。
func TestStallGuardStallFires(t *testing.T) {
	blocking := newFakeStream([]byte("head"), true) // 4 字节后挂死
	rc := newStallGuard(blocking, 60*time.Millisecond)
	defer rc.Close()

	buf := make([]byte, 64)
	n, err := rc.Read(buf) // 先读走头部 4 字节
	if err != nil || n != 4 {
		t.Fatalf("头部读取: n=%d err=%v", n, err)
	}
	start := time.Now()
	_, err = rc.Read(buf)
	if !errors.Is(err, ErrDownloadStalled) {
		t.Fatalf("停滞未中断: err=%v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("中断太慢: %v", elapsed)
	}
}

// TestStallGuardClientStallReclaims 客户端停滞（调用方不读）⇒ 看门狗到期关
// 底层流，泵解除阻塞退出——挂死连接被回收。
func TestStallGuardClientStallReclaims(t *testing.T) {
	f := newFakeStream(bytes.Repeat([]byte("x"), 1024), true)
	rc := newStallGuard(f, 60*time.Millisecond)
	// 故意不读：泵读走首个 chunk 后阻塞在管道写端 ⇒ 看门狗到期回收
	select {
	case <-f.closed:
		// 底层流已被回收 ✓
	case <-time.After(2 * time.Second):
		t.Fatal("底层流未被回收，挂死连接泄漏")
	}
	_ = rc.Close()
}

// TestStallGuardDisabled 配置 <=0 ⇒ 不限：原样返回底层流（零开销）。
func TestStallGuardDisabled(t *testing.T) {
	f := newFakeStream([]byte("abc"), false)
	for _, stall := range []time.Duration{0, -time.Second} {
		if got := newStallGuard(f, stall); got != f {
			t.Fatalf("stall=%v 应原样返回底层流", stall)
		}
	}
	// nil 流不 panic
	if got := newStallGuard(nil, time.Second); got != nil {
		t.Fatalf("nil 流应原样返回")
	}
}

// TestStallGuardCloseIdempotent 并发 Close 底层只关一次；Close 后读端收口。
func TestStallGuardCloseIdempotent(t *testing.T) {
	f := newFakeStream([]byte("abc"), true)
	rc := newStallGuard(f, time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = rc.Close()
		}()
	}
	wg.Wait()
	if c := f.closeCount(); c != 1 {
		t.Fatalf("底层流关闭次数 = %d，应恰好 1 次", c)
	}
}

// TestStallGuardUnderlyingError 底层读错误（非 EOF）原样透传给读端。
func TestStallGuardUnderlyingError(t *testing.T) {
	boom := errors.New("storage boom")
	f := &errStream{err: boom}
	rc := newStallGuard(f, time.Second)
	defer rc.Close()
	_, err := io.ReadAll(rc)
	if !errors.Is(err, boom) {
		t.Fatalf("底层错误未透传: %v", err)
	}
}

// errStream 首读即报错的流。
type errStream struct{ err error }

func (e *errStream) Read(p []byte) (int, error) { return 0, e.err }
func (e *errStream) Close() error               { return nil }
