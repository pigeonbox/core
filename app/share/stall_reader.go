package share

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/oekit/concurrency/async"
	"github.com/pigeonbox/core/conf"
)

// ErrDownloadStalled 下载停滞超时（download.download_timeout，秒）：传输中连续
// N 秒无任何字节流动（存储卡死 / 客户端消失），守卫主动中断下载回收连接。
// 注：响应已进入流式写出后无法再改状态码，客户端表现为传输中断/不完整下载。
var ErrDownloadStalled = errors.New("下载停滞超时：连续无字节流动")

// errStallPumpAborted 停滞守卫泵协程异常退出（含 panic 被 GoSafe 收编）时
// 读端拿到的兜底错误——防读端因写端未关而永久阻塞。
var errStallPumpAborted = errors.New("下载流泵异常终止")

// downloadStallTimeout 停滞守卫时长。委托 conf.DownloadStallTimeout（全局配置
// 读收口在 conf 属主包——规则8 棘轮的定义处豁免面；本包不各自直读全局配置）。
// 动态语义：每次开流现读，管理台保存后对新下载立即生效。
func downloadStallTimeout() time.Duration {
	return conf.DownloadStallTimeout()
}

// newStallGuard 给下载读流套「停滞守卫」：stall 内无任何字节流过即中断下载
// （无论停滞来自后端存储还是客户端不读）。stall <= 0 或 rc == nil 原样返回。
//
// 实现：io.Pipe + GoSafe 泵 + time.AfterFunc 看门狗。泵每次从底层取到字节即
// 续期看门狗；看门狗到期对写端 CloseWithError(ErrDownloadStalled) ⇒ 读端 Read
// 返回该错误，同时唤醒可能阻塞在写端的泵（管道背压 ⇒ 客户端停滞同样计时）。
//
// 返回的 ReadCloser：Close 关闭底层流、回收看门狗并解除泵的阻塞；底层流只关一次。
func newStallGuard(rc io.ReadCloser, stall time.Duration) io.ReadCloser {
	if stall <= 0 || rc == nil {
		return rc
	}
	g := &stallGuard{rc: rc}
	g.pr, g.pw = io.Pipe()
	g.timer = time.AfterFunc(stall, func() { _ = g.pw.CloseWithError(ErrDownloadStalled) })
	async.GoSafe(func() {
		// 清理 defer 兜底关写端：正常出口先 Close/CloseWithError（首个错误保留，
		// io.Pipe 语义 CloseWithError 不覆盖已有错误），panic 被 GoSafe 收编前
		// 本 defer 先执行 ⇒ 读端不会挂死。
		defer func() {
			g.timer.Stop()
			g.closeRC()
			_ = g.pw.CloseWithError(errStallPumpAborted)
		}()
		buf := make([]byte, 64<<10)
		for {
			n, rerr := g.rc.Read(buf)
			if n > 0 {
				g.timer.Reset(stall) // 有字节流动 ⇒ 续期
				if _, werr := g.pw.Write(buf[:n]); werr != nil {
					return // 读端已关（客户端断开/停滞超时/Close）
				}
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					_ = g.pw.Close()
				} else {
					_ = g.pw.CloseWithError(rerr)
				}
				return
			}
		}
	})
	return g
}

// stallGuard 停滞守卫读端（实现 io.ReadCloser）。
type stallGuard struct {
	pr    *io.PipeReader
	pw    *io.PipeWriter
	rc    io.ReadCloser
	timer *time.Timer

	rcCloseOnce sync.Once
}

func (g *stallGuard) Read(p []byte) (int, error) { return g.pr.Read(p) }

// Close 回收守卫：停看门狗、关读端管道（解除泵的阻塞写）、关底层流。
// 底层流经 sync.Once 只关一次（泵退出路径与调用方 Close 可能并发）。
func (g *stallGuard) Close() error {
	g.timer.Stop()
	_ = g.pr.Close()
	g.closeRC()
	return nil
}

func (g *stallGuard) closeRC() {
	g.rcCloseOnce.Do(func() { _ = g.rc.Close() })
}
