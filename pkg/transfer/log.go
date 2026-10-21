// Package transfer 提供传输日志（上传/下载）的异步记录。
//
// 背景：TransferLog 模型/DAO/前端页面早已存在，但全库无任何写入方，
// 传输日志页与趋势统计的下载序列一直是空数据——本包是写入侧的唯一收口。
// 记录失败只记运行日志，绝不影响主流程。
//
// 落盘能力经 Sink 注入（bootstrap 以 dao 仓储桥接）：pkg 是最底层共享库，
// 不直接依赖 repo 层。
package transfer

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/oekit/concurrency/async"
	"github.com/pigeonbox/core/pkg/logger"
)

// Sink 传输日志落盘能力（repo/db/dao 的仓储实现，bootstrap 装配注入）。
type Sink interface {
	Create(ctx context.Context, e Entry) error
}

var sink Sink // nil = 未装配，记录丢弃（记录绝不影响主流程）

// inFlight 在飞落盘计数；Flush 排水用（Record 与 Flush 一一配对）。
var inFlight sync.WaitGroup

// SetSink 注入落盘实现（bootstrap 装配调用；须先于任何服务流量）。
func SetSink(s Sink) { sink = s }

// Flush 等待在飞日志落盘，至多等待 d。优雅停机与测试清理用——
// 异步 goroutine 持有 DB/文件句柄写盘，调用方（如测试的 TempDir 清理）
// 必须先排水再释放底层资源，否则出现"readonly database/目录非空"竞争。
func Flush(d time.Duration) {
	done := make(chan struct{})
	go func() {
		inFlight.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// 操作类型常量（与 model.TransferLog.Operation 语义一致）
const (
	OpUpload   = "upload"
	OpDownload = "download"
)

// Entry 一次传输的归因信息。UserID/APIKeyID 为 nil 表示匿名/JWT 认证。
type Entry struct {
	Operation  string // upload | download
	FileCodeID uint
	Code       string
	FileName   string
	FileSize   int64
	UserID     *uint
	APIKeyID   *uint // 非 nil 表示该操作经用户级 API Key 认证（泄露排查归因用）
	Username   string
	IP         string
	DurationMs int64
}

// Record 异步记录一次传输（GoSafe 兜底 panic，不影响主流程）。
func Record(e Entry) {
	// sink **同步捕获**：不在 goroutine 里读全局。
	// 读全局的后果：goroutine 真正执行时全局可能已被换掉（配置热重建换 sink，
	// 或测试里另一个用例换了 sink）→ 这条日志会被写进**别的** sink/库，
	// 表现为 "no such table: xxx" 与 TempDir 清理竞争（2026-07-22 实测）。
	// 语义上也更对：这条记录属于「记录发生的那一刻」的 sink。
	s := sink
	inFlight.Add(1)
	async.GoSafe(func() {
		defer inFlight.Done()
		if s == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.Create(ctx, e); err != nil {
			logger.Warn("transfer log write failed",
				zap.String("operation", e.Operation), zap.String("code", e.Code), zap.Error(err))
		}
	})
}
