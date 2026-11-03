// quota.go 匿名上传 per-IP 日配额（治理 2026-10-03）。
//
// 此前配额只对登录用户生效（share.CreateShare → CheckQuota），匿名分片/直传
// 完全绕过——刷爆存储的主通道。本文件以"自然日"为窗口对匿名来源 IP 计数
// （次数 + 字节双维度，任一超限即拒）：
//   - Redis 可用：INCR + 当日剩余秒数 EXPIRE（多实例共享计数）
//   - 无 Redis：进程内 map + 日期轮转（单实例语义，重启清零）
package gate

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oekit/pkg/timex"
	"github.com/oekit/redisx"
	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/metrics"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// dailyQuota 匿名日配额计数器。
type dailyQuota struct {
	// rdb 原子持有（**不放在 dailyQuota 结构体内直接赋值**）。
	//
	// ⚠️ 接口字段的读写**不是原子的**（接口值 = 类型指针 + 数据指针，16 字节）：
	// SetQuotaRedis 写与请求路径读并发时可能读到**撕裂**的接口值
	// ⇒ Go runtime panic（invalid memory address）。故用 atomic.Pointer 存
	// 具体类型 *redis.Client，读侧 Load() 一次拿到一致快照。
	rdb atomic.Pointer[redis.Client]

	mu      sync.Mutex
	mem     map[string]*quotaEntry // key: ip -> 当日计数
	memDate string
}

type quotaEntry struct {
	count int64
	bytes int64
}

var quota = &dailyQuota{mem: map[string]*quotaEntry{}}

// SetQuotaRedis 注入 Redis（bootstrap 调用；nil = 内存模式）。
// nil 归一化：typed-nil 接口会骗过内存模式守卫（!= nil 判真）。
func SetQuotaRedis(rdb *redis.Client) {
	quota.rdb.Store(rdb)
}

// QuotaLimits 当前生效的匿名日配额（0 = 不限）。
func QuotaLimits() (count int64, bytes int64) {
	cfg := conf.GetGlobalConfig()
	if cfg == nil {
		return 0, 0
	}
	return cfg.Upload.AnonymousDailyCount, cfg.Upload.AnonymousDailyBytes
}

// CheckAnonymousQuota 匿名上传配额检查并计数（仅对匿名请求调用）。
// 超限返回 *GateError（业务码 10014）；不限或登录请求恒通过。
func CheckAnonymousQuota(ctx context.Context, ip string, addBytes int64) error {
	maxCount, maxBytes := QuotaLimits()
	if maxCount <= 0 && maxBytes <= 0 {
		return nil // 未启用
	}
	if ip == "" {
		return nil // 拿不到来源不误伤（限流中间件已在更外层兜底）
	}

	day := time.Now().Format("20060102")
	var curCount, curBytes int64
	var err error
	if rdb := quota.rdb.Load(); rdb != nil {
		curCount, curBytes, err = quota.incrRedis(ctx, rdb, ip, day, addBytes)
	} else {
		curCount, curBytes, err = quota.incrMem(ip, day, addBytes)
	}
	if err != nil {
		// 计数基础设施故障不阻断业务（与配额"运营约束"定位一致），仅告警
		logger.Warn("anonymous quota counter failed, allow", zap.String("ip", ip), zap.Error(err))
		return nil
	}

	if maxCount > 0 && curCount > maxCount {
		metrics.RecordRejected(metrics.RejectAnonQuota)
		return newGateErr(errcode.CodeAnonymousQuota,
			"今日上传次数已达上限（%d 次/日），请明日再试或登录后使用", maxCount)
	}
	if maxBytes > 0 && curBytes > maxBytes {
		metrics.RecordRejected(metrics.RejectAnonQuota)
		return newGateErr(errcode.CodeAnonymousQuota,
			"今日上传流量已达上限（%d 字节/日），请明日再试或登录后使用", maxBytes)
	}
	return nil
}

// incrRedis Redis 侧计数。rdb 由调用方从 atomic 取出后传入——
// **不在这里读 d.rdb**，保证同一次调用内用的是同一个一致快照。
//
// 计数原语收编到 **oekit/redisx** 的 `Counter.IncrWithTTL`（Lua 原子），
// 与 pkg/middleware 的限流/锁定计数同一原语（2026-10-28 收编那轮的漏网点：
// 本文件当时仍是手搓 TxPipeline「Incr + Expire」）。
//
// 等价性核对（与旧 TxPipeline 版逐条对齐）：
//   - TTL = 当日结束 + 1h 缓冲（跨时区/时钟偏移兜底）的**绝对过期**：旧版每次
//     重设本就幂等，新版「已有 TTL 不续期」得到同一绝对过期时刻；
//   - 键名不变（redisx 纯包装默认无前缀，见 NewClientFromClient）；
//   - 原子性从「两键一条 pipeline」降为**单键原子**：count 与 bytes 是两个
//     独立上限，互不需要跨键事务；极端失败窗口下两维计数可能差一次，
//     计数基础设施故障本就 fail-open（见 CheckAnonymousQuota），可接受。
func (d *dailyQuota) incrRedis(ctx context.Context, rdb *redis.Client, ip, day string, addBytes int64) (int64, int64, error) {
	key := fmt.Sprintf("pb:quota:%s:%s", ip, day)
	ttl := timeUntilMidnight() + time.Hour
	rc := redisx.NewClientFromClient(rdb)
	cnt, err := rc.NewCounter(key+":count").IncrWithTTL(ctx, 1, ttl)
	if err != nil {
		return 0, 0, err
	}
	byt, err := rc.NewCounter(key+":bytes").IncrWithTTL(ctx, addBytes, ttl)
	if err != nil {
		return 0, 0, err
	}
	return cnt, byt, nil
}

func (d *dailyQuota) incrMem(ip, day string, addBytes int64) (int64, int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.memDate != day {
		d.mem = map[string]*quotaEntry{} // 日期轮转
		d.memDate = day
	}
	e := d.mem[ip]
	if e == nil {
		e = &quotaEntry{}
		d.mem[ip] = e
	}
	e.count++
	e.bytes += addBytes
	return e.count, e.bytes, nil
}

// timeUntilMidnight 距次日零点的剩余时长。
//
// 收编 oekit/pkg/timex.EndOfDay（2026-11-03）：当日结束＝次日零点−1ns，
// 与手搓 time.Date(..., 24, 0, ...) 的差被调用点 TTL 的小时级宽限吸收
// （EXPIRE 秒级截断不可见），行为等价。
func timeUntilMidnight() time.Duration {
	return time.Until(timex.EndOfDay(time.Now()))
}
