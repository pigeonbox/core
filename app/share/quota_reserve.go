package share

import (
	"context"
	"fmt"
	"sync"

	"github.com/pigeonbox/contracts/errcode"

	"github.com/pigeonbox/core/pkg/gate"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/metrics"
)

// 配额预留账本（core#169）。
//
// 缺陷：配额检查「读合计 → 判定 → 调用方之后才 insert」无事务无锁（check-then-act）
// ⇒ N 个并发 CreateShare 各自读到同一 used、全部放行后各自 insert ⇒ 落库合计可达
// quota + (N-1)×size。站点级（storage.quota）与用户级（CheckQuota）两条链同病。
//
// 修法（预留-提交-回滚）：判定在**本账本临界区**内完成并同时占位在途字节
// （site/user 两个维度），调用方在 insert 完成（或失败回滚）后释放。任一时刻
// 「已提交合计 + 在途预留」是真实占用的上界：
//   - 预留后、插入前：别的判定看得见在途字节 ⇒ 不会放行超额；
//   - 插入提交后、释放前：同一笔字节被双计（保守方向——短暂多拒，绝不超放）；
//   - 失败路径/panic：defer 释放，无残留（测试断言账本归零）。
//
// 为什么必须在临界区内读合计：合计读到临界区外 ⇒ 读到的 used 可能是别的请求
// 提交前的陈旧值，而它的在途预留可能已释放 ⇒ 双双漏判（必须 SUM与占位原子复合）。
//
// 用户级把在途预留折进 addBytes（totalSize+inflight）交给 quotaChecker.CheckQuota
// ——该方法的读-比-放行由此在临界区内成为原子判定，错误类型/文案/fail-open 口径
// 全部保持在 app/user 单源（见其注释的调用契约）。
//
// 覆盖面：**进程内互斥**（单机 standalone 的全部形态，以及多副本各自的同刻并发）
// ——与 core#151 uploadlock 同一边界；跨副本（deployment.mode=public×N）同刻并发
// 不在此原语覆盖内，彻底收口需 DB 侧序列化点（守卫行/顾问锁，方言各异），
// 见本仓 uploadlock 注释同款说明。
var quotaReserve = struct {
	mu   sync.Mutex
	site int64
	user map[uint]int64
	// dir 目录维度（core#159）：key = 目录策略 root（EvalSymlinks 解析后的形态），
	// value = 该目录的**在途预留**字节。与 site/user 维度**共用同一把锁** ⇒
	// 目录维度的「读存活合计 → 比限额 → 占位」是原子判定。
	dir map[string]int64
}{user: make(map[uint]int64), dir: make(map[string]int64)}

// reserveUploadCaps 配额判定 + 预留：通过则返回幂等 release（调用方 defer）。
//
// 判定项与口径与改造前逐条一致（站点级物理口径 / 单次上限 / 用户级配额 /
// 各自 fail-open），差别只有一条：判定与 insert 之间不再裸奔。
func (s *Service) reserveUploadCaps(ctx context.Context, userID *uint, totalSize int64) (release func(), err error) {
	quotaReserve.mu.Lock()
	defer quotaReserve.mu.Unlock()

	// 站点级全局存储配额（storage.quota，0=不限）：全通道统一闸口
	// （直传/分片完成/预签名完成/本地导入/多文件均经此处）。统计口径=
	// **物理占用**（含回收站软删——purge 前仍占盘；评审 D01）。原存活口径
	// 漏计软删 ⇒ 站点配额虚低。未完成分片会话暂不计入。统计故障时 fail-open
	// （配额是治理项而非安全项，不因统计异常拒绝所有上传）。
	if quota := s.storageQuota(); quota > 0 {
		if used, serr := s.fileCodeRepo.GetPhysicalObjectUsage(ctx); serr == nil {
			if used+quotaReserve.site+totalSize > quota {
				metrics.RecordRejected(metrics.RejectQuota)
				return nil, &GlobalQuotaExceededError{Used: used, Quota: quota}
			}
		} else {
			logger.CtxWarn(ctx, "全局存储配额统计失败，本次跳过配额检查", "err", serr)
		}
	}
	// 单用户单次上传上限（users.max_upload_size，非累计语义，无预留面）
	if userID != nil && s.userService != nil {
		if capSize := s.userService.GetUploadSizeCap(ctx, *userID); capSize > 0 {
			// typed error（CodeTooLarge→413）：此前为普通 error，NewTypedError
			// 兜底 CodeInternal，前端显示「服务器内部错误」掩盖超限真相（2026-10-19 事故）。
			if cerr := gate.CheckUploadSizeCap(userID, totalSize, capSize); cerr != nil {
				return nil, cerr
			}
		}
	}
	// 用户级存储配额：把**在途预留**折进 addBytes ⇒ 临界区内的读-比-放行即原子
	//（CheckQuota 内部读到的合计不含在途，故必须由调用方补上 inflight）。
	if userID != nil && s.quotaChecker != nil {
		if qerr := s.quotaChecker.CheckQuota(ctx, *userID, totalSize+quotaReserve.user[*userID]); qerr != nil {
			metrics.RecordRejected(metrics.RejectQuota)
			return nil, qerr
		}
	}

	// 预留占位：自此到 release 之间，本笔字节对后续判定可见
	quotaReserve.site += totalSize
	if userID != nil {
		quotaReserve.user[*userID] += totalSize
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			quotaReserve.mu.Lock()
			defer quotaReserve.mu.Unlock()
			quotaReserve.site -= totalSize
			if userID != nil {
				quotaReserve.user[*userID] -= totalSize
				if quotaReserve.user[*userID] <= 0 {
					delete(quotaReserve.user, *userID)
				}
			}
		})
	}, nil
}

// DirectoryQuotaExceededError 目录配额超限（core#159）。
//
// typed error 约定：实现 ErrCode() int ⇒ handler 侧透传业务码
// errcode.CodeStorageQuota（30002，HTTP 507 Insufficient Storage）——与站点级
// 存储配额超限（GlobalQuotaExceededError）同码同族：两者都是「存储额度用完」，
// 前端可共用一条提示文案与重试路径。
//
// 文案必须**点名具体目录与限额**：运维要能直接定位到是哪条目录策略挡下的，
// 不必再去翻配置（core#159 的运维可定位性要求）。
type DirectoryQuotaExceededError struct {
	// DirRoot 命中的目录策略 root（已解析形态）
	DirRoot string
	// Used 该目录当前存活分享合计字节
	Used int64
	// Quota 该目录的配额上限字节
	Quota int64
	// Size 本次请求的字节数
	Size int64
}

func (e *DirectoryQuotaExceededError) Error() string {
	return fmt.Sprintf(
		"目录 %s 的分享配额已满（该目录存活分享 %d 字节 + 本次 %d 字节，上限 %d 字节），请清理该目录的过期分享或调整目录配额",
		e.DirRoot, e.Used, e.Size, e.Quota)
}

func (e *DirectoryQuotaExceededError) ErrCode() int { return errcode.CodeStorageQuota }

// reserveDirectoryQuota 目录配额判定 + 预留（core#159）：通过则返回幂等 release
// （调用方 defer），超限返回 *DirectoryQuotaExceededError。
//
// 与 site/user 维度**同一把锁**（quotaReserve.mu）⇒ 目录维度的
// 「读存活合计 → 比限额 → 占位」是**原子判定**：在途字节对后续判定可见 ⇒
// 并发导入不会超放（与 core#169 的预留-提交-回滚同款机制、同款边界）。
//
// 为什么必须在临界区内读合计：合计读到临界区外 ⇒ 读到的 used 可能是别的请求
// 提交前的陈旧值，而它的在途预留可能已释放 ⇒ 双双漏判（必须 SUM 与占位原子复合）。
//
// 用量口径 = **存活**分享（未软删、未过期），与站点级物理占用口径**故意不同**
// （见 dao.GetDirectoryUsage 的口径说明：站点量「盘上还占多少」、目录量
// 「这个目录对外分享出去多少」）。
//
// 覆盖面与 site/user 维度**同款**：**进程内互斥**（单机 standalone 的全部形态，
// 以及多副本各自的同刻并发）——与 core#151 uploadlock 同一边界；跨副本
// （deployment.mode=public×N）同刻并发**不在**此原语覆盖内，彻底收口需
// DB 侧序列化点（守卫行/顾问锁，方言各异），见本仓 uploadlock 注释同款说明。
// 本函数不承诺跨副本安全。
//
// 统计故障 ⇒ fail-open（配额是治理项而非安全项，与站点级口径一致：不因统计
// 异常拒绝所有导入），但**绝不静默**：告警留痕。
func (s *Service) reserveDirectoryQuota(ctx context.Context, dirRoot string, quota int64, size int64) (func(), error) {
	if quota <= 0 {
		return func() {}, nil
	}
	quotaReserve.mu.Lock()
	defer quotaReserve.mu.Unlock()

	used, err := s.fileCodeRepo.GetDirectoryUsage(ctx, dirRoot)
	if err != nil {
		logger.CtxWarn(ctx, "directory usage query failed, skip directory quota check",
			"dir_root", dirRoot, "err", err)
		return func() {}, nil
	}
	inflight := quotaReserve.dir[dirRoot]
	if used+inflight+size > quota {
		metrics.RecordRejected(metrics.RejectQuota)
		return nil, &DirectoryQuotaExceededError{DirRoot: dirRoot, Used: used, Quota: quota, Size: size}
	}

	// 预留占位：自此到 release 之间，本笔字节对后续判定可见
	quotaReserve.dir[dirRoot] = inflight + size
	var once sync.Once
	return func() {
		once.Do(func() {
			quotaReserve.mu.Lock()
			defer quotaReserve.mu.Unlock()
			quotaReserve.dir[dirRoot] -= size
			if quotaReserve.dir[dirRoot] <= 0 {
				delete(quotaReserve.dir, dirRoot)
			}
		})
	}, nil
}
