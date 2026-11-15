package dblock

// dblock.go —— **DB 级命名互斥闸**（跨副本；core#169 / core#191）。
//
// # 为什么需要它
//
// 进程内互斥（internal/uploadlock 的条纹闸、配额闸此前完全没有）在多副本部署
// （deployment.mode=public × N）下互不可见：两个 Pod 各自加各自的锁，同一把
// 「逻辑锁」被并发持有。凡是「读状态 → 判定 → 写入」的临界段，跨副本并发都会
// 打穿判据：
//   - core#169：站点/用户配额「读合计 → 比上限 → 插入」非原子 ⇒ 并发上传超额；
//   - core#191：同一 upload_id 的上传完成面在两个副本并发执行 ⇒ 重复建分享。
//
// ⇒ 本原语提供**跨副本**的命名互斥，底座按方言分派：
//
//   - MySQL：会话级 GET_LOCK / RELEASE_LOCK（钉住一条 *sql.Conn 保会话）；
//   - PostgreSQL：会话级 pg_advisory_lock / pg_advisory_unlock（同上）；
//   - SQLite（及其他方言）：进程内条纹互斥——SQLite 只用于 standalone 单进程
//     部署（多副本是 MySQL/PG 的部署形态，见 bootstrap 部署约束），进程内互斥
//     在该形态下与 DB 级互斥语义等价。
//
// # 为什么不用行锁/唯一约束
//
// 配额合计是聚合值，行锁锁不住「未来行」；upload_id 唯一约束与属主语义冲突
// （upload_id 可由 file_hash 派生，不同属主持同一 upload_id 是合法形态）。
// 命名互斥不依赖表结构、无需迁移、进程崩溃（连接断开）自动释放，是最小形态。
//
// # 持锁纪律
//
// 临界段要短：持锁期间不要做慢 IO（大文件合并/哈希等在取得互斥权**之前**完成
// 或明确接受后到方的等待上限）。锁是**排队**语义（等待而非即拒），等待上限
// dlockWait；ctx 取消/超时立即放弃。
//
// 正确性分层（core#191 方案第 4 步）：进程内锁（uploadlock 等）降级为性能优化
// 层（减少无谓的 DB 往返），正确性由本闸承担——移除进程锁用例仍绿。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"sync"
	"time"

	"gorm.io/gorm"
)

// dlockWait 取锁等待上限。完成面/配额临界段都在秒级内；超时 = 临界段异常
// 长或持有者僵死，返回错误让调用方按失败处理（客户端可重试），不做无限等待。
const dlockWait = 2 * time.Minute

// ErrLockTimeout 取锁等待超时（调用方可按「有并发方在跑」向用户解释）。
var ErrLockTimeout = errors.New("dblock: acquire lock timeout (another replica may be working)")

// DLock 跨副本命名互斥闸。构造一次复用（goroutine 安全）。
type DLock struct {
	dialect string
	sqlDB   *sql.DB
}

// NewDLock 从 gorm 句柄构造闸（取底层 *sql.DB 与方言名）。
func NewDLock(gormDB *gorm.DB) *DLock {
	l := &DLock{}
	if gormDB != nil {
		if gormDB.Dialector != nil {
			l.dialect = gormDB.Name() // = Dialector.Name()（gorm 透传）
		}
		l.sqlDB, _ = gormDB.DB()
	}
	return l
}

// 闸名约定（调用方统一经这里取，避免各处散落字符串）。

// QuotaGateKey 配额闸：站点/用户配额的「合计 → 判定 → 写入」临界段（core#169）。
func QuotaGateKey() string { return "pbc:quota_gate" }

// UploadCompletionKey 上传完成面闸：同一 upload_id 的 Complete 临界段（core#191）。
func UploadCompletionKey(uploadID string) string { return "pbc:upload_complete:" + uploadID }

// Lock 取得 name 的互斥权；返回幂等 release（调用方 defer）。
// 等待上限 dlockWait，ctx 取消/超时立即返回错误（此时未进入临界段）。
func (l *DLock) Lock(ctx context.Context, name string) (func(), error) {
	if l == nil || l.sqlDB == nil {
		return nil, errors.New("dblock: nil db handle")
	}
	switch l.dialect {
	case "mysql":
		return l.lockMySQL(ctx, name)
	case "postgres":
		return l.lockPostgres(ctx, name)
	default:
		// sqlite 及其他：进程内条纹互斥（standalone 单进程部署形态，语义等价）
		return lockProcessStripe(ctx, name)
	}
}

// ── MySQL：会话级 GET_LOCK ────────────────────────────────────────────────

func (l *DLock) lockMySQL(ctx context.Context, name string) (func(), error) {
	identity := mysqlLockName(name)
	conn, err := l.sqlDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("dblock: pin conn: %w", err)
	}
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", identity, int(dlockWait.Seconds())).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("dblock: GET_LOCK: %w", err)
	}
	if !got.Valid || got.Int64 != 1 {
		_ = conn.Close()
		return nil, ErrLockTimeout
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			// 释放用独立 ctx：调用方的请求 ctx 可能已取消，RELEASE_LOCK 仍要送达
			bg := context.WithoutCancel(ctx)
			_, _ = conn.ExecContext(bg, "SELECT RELEASE_LOCK(?)", identity)
			_ = conn.Close()
		})
	}, nil
}

// mysqlLockName GET_LOCK 名长上限 64 字符：短名直用，长名（upload_id 可达
// 128 字符）fnv64 哈希成 16 hex。确定性映射：同名恒同锁。
func mysqlLockName(name string) string {
	full := "pbc:" + name
	if len(full) <= 60 {
		return full
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return "pbc:" + strconv.FormatUint(h.Sum64(), 16)
}

// ── PostgreSQL：会话级 advisory lock ─────────────────────────────────────

func (l *DLock) lockPostgres(ctx context.Context, name string) (func(), error) {
	conn, err := l.sqlDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("dblock: pin conn: %w", err)
	}
	// pg_advisory_lock 无超时参数 ⇒ 用 ctx 超时上限（超时驱动发 cancel，等待中止）
	lockCtx, cancel := context.WithTimeout(ctx, dlockWait)
	defer cancel()
	// hashtextextended：64 位哈希（int4 的 hashtext 碰撞面更大）；锁键与名字
	// 无长度耦合（upload_id 128 字符也直进）。
	if _, err := conn.ExecContext(lockCtx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", name); err != nil {
		_ = conn.Close()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrLockTimeout
		}
		return nil, fmt.Errorf("dblock: pg_advisory_lock: %w", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			bg := context.WithoutCancel(ctx)
			_, _ = conn.ExecContext(bg, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", name)
			_ = conn.Close()
		})
	}, nil
}

// ── 进程内条纹互斥（sqlite 等单进程方言）─────────────────────────────────

// stripes 条纹数：键集上界恒 ≤64（upload_id 高基数，直接当键会慢性泄漏——
// 与 internal/uploadlock 同一结论）。
const dlockStripes = 64

var dlockStripeMu [dlockStripes]sync.Mutex

func stripeIndex(name string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return int(h.Sum32() % dlockStripes)
}

func lockProcessStripe(ctx context.Context, name string) (func(), error) {
	mu := &dlockStripeMu[stripeIndex(name)]
	// TryLock 轮询：ctx 可中断的互斥获取（裸 Mutex.Lock 不感知取消）
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(dlockWait)
	defer deadline.Stop()
	for {
		if mu.TryLock() {
			var once sync.Once
			return func() { once.Do(mu.Unlock) }, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, ErrLockTimeout
		case <-ticker.C:
		}
	}
}
