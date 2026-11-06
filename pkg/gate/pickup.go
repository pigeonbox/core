// pickup.go 取件闸门：过期 → 管控 → 就绪 → 密码/防爆破锁定 的统一判定。
//
// 历史：同一条「取件前置校验」在四处各写一份且规格互不一致——
// share/select、share/download、anonymous.Retrieve 的锁定在 handler 层
// （scope 各为 pickup/download/anon，计数互不相通，同一口令在四个入口
// 各有独立爆破预算），preview 的锁定在域层且用 FormatLockKey（不做取件码
// 大小写折叠，存在变体绕过锁定漏洞）。2026-10-10 收口于此：
//   - 单一 scope "pickup"：四入口共享 (IP, code) 失败计数；
//   - 统一 LookupLockKey（折叠开启时大小写变体同键）；
//   - 判定顺序、锁定检查、失败记账、成功清零只此一份。
//
// 域/handler 侧职责不变：把 model.FileCode 映射为 PickupState（pkg 层不得
// import repo，架构守卫规则 1），把 PickupVerdict 映射为各自的错误类型与
// HTTP 响应形态（select 空密码提示不计数、preview 空密码计数等入口差异经
// MissingCountsFailure 表达）。
package gate

import (
	"context"

	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/utils"
)

// PickupScope 统一锁定 scope：取件/下载/预览/匿名取件四入口共享失败计数。
const PickupScope = "pickup"

// PickupLockKey 统一锁定键（scope 固定 "pickup"；取件码折叠开启时大小写
// 变体同键，与 LookupLockKey 语义一致）。handler 预检与枚举记账用同一键。
func PickupLockKey(clientIP, code string) string {
	return middleware.LookupLockKey(PickupScope, clientIP, code)
}

// PickupEnumScope 取件码**枚举**的按 IP 维度 scope。
//
// 为什么需要独立维度（2026-11-06 审计 P2）：既有统一键是 (IP, 码)，而枚举攻击的
// 特征恰恰是**每次换一个码** ⇒ 每个码各自一份计数，谁也到不了阈值 ⇒ 记账写了、
// 却永远锁不上人 ⇒ 枚举防护形同虚设（"记了但没人统计"）。枚举的对手是 IP，
// 不是码 ⇒ 同一 IP 在窗口内的累计失败达到阈值即锁定该 IP（拒绝一切码）。
//
// 代价（已知并接受）：同一出口 IP 后的多人（NAT/办公网）共享这份预算；阈值是
// security.lockout 的全局口径（默认 10 次/300s，锁 600s），正常用户不会在 5 分钟
// 内连试 10 个**不同**的码，故可接受——否则枚举面只剩限流桶这一道软防线。
const PickupEnumScope = "pickup-enum"

// PickupEnumLockKey 按 IP 的枚举锁定键（不含码维度——这正是它与
// PickupLockKey 的差别）。
func PickupEnumLockKey(clientIP string) string {
	return middleware.FormatLockKey(PickupEnumScope, clientIP)
}

// RecordPickupFailure 记一次「码不存在」类失败（供到达不了 CheckPickup 的
// 路径做枚举防护记账）。
//
// 同时记两个维度：① (IP, 码) 保持与历史/其他入口一致的口径；② **(IP) 枚举维度**
// ——后者才是枚举攻击真正会触发的那个计数（换码也能累计）。空 IP 只记码维度
// （无 IP 时所有匿名来源会挤进同一个键，锁一个人等于锁全网）。
func RecordPickupFailure(ctx context.Context, clientIP, code string) {
	lock := middleware.GetDefaultLockout()
	_, _ = lock.RecordFailure(ctx, PickupLockKey(clientIP, code))
	if clientIP != "" {
		_, _ = lock.RecordFailure(ctx, PickupEnumLockKey(clientIP))
	}
}

// PickupEnumLockedRemain 查询按 IP 枚举维度的剩余锁定秒数。
func PickupEnumLockedRemain(ctx context.Context, clientIP string) (int, bool) {
	if clientIP == "" {
		return 0, false
	}
	return middleware.GetDefaultLockout().CheckLocked(ctx, PickupEnumLockKey(clientIP))
}

// PickupLockedRemain 查询入口锁定的剩余秒数（handler 快速 429 预检用；
// 未锁定返回 (0, false)）。
//
// **两个维度取较长**（2026-11-06）：(IP, 码) 与按 IP 的枚举维度任一命中都该 429
// ——只查前者会让 handler 预检漏掉"枚举已把该 IP 锁上"的情形（预检放行、
// 域内闸门再拒，白跑一次业务查询）。
func PickupLockedRemain(ctx context.Context, clientIP, code string) (int, bool) {
	lock := middleware.GetDefaultLockout()
	remain, locked := lock.CheckLocked(ctx, PickupLockKey(clientIP, code))
	if enumRemain, enumLocked := PickupEnumLockedRemain(ctx, clientIP); enumLocked && enumRemain > remain {
		return enumRemain, true
	}
	return remain, locked
}

// PickupState 取件目标分享的状态快照（域侧从 model.FileCode 映射；
// Blocked 由 IsBlockedShare() 判定，BlockedStatus 仅用于错误映射透传）。
type PickupState struct {
	// Expired **只表示时间过期**（次数耗尽在交付面单独判定）。
	// ⚠️ 2026-10-26 审计订正：旧注释写「时间或次数已耗尽」⇒ 会让人照着把
	// 次数语义**合流回这个闸门** ⇒ 刚修好的 20004「超过取件次数」/
	// 20007「取件码已用完」会被压回 404「不存在/已过期」（2026-10-24 那轮
	// 踩过的坑）。⇒ 次数耗尽由 model.IsCountExhausted() 判定，并在**交付面**
	// 返回独立错误码，**不在本闸门判**。
	Expired       bool   // 时间过期（不含次数耗尽）
	Blocked       bool   // 管控拒绝态（禁用/待审核）
	BlockedStatus string // 原始状态串（blocked/pending_review），Blocked=true 时有效
	NotReady      bool   // 登记占位未回填物理文件（匿名取件通道语义）
	RequireAuth   bool   // 密码保护
	PasswordHash  string // bcrypt 哈希（RequireAuth=true 而哈希空 = 历史脏数据，按拒绝处理）
}

// PickupVerdict 闸门判定结果。
type PickupVerdict int

const (
	PickupAllowed         PickupVerdict = iota // 放行
	PickupUnavailable                          // 不存在/已过期（各面自行统一 404 语义）
	PickupBlocked                              // 管控拒绝态（携带 BlockedStatus）
	PickupNotReady                             // 未完成上传登记
	PickupLocked                               // 防爆破锁定态（携带 LockRemain 秒）
	PickupPasswordMissing                      // 需要密码但未提供（未记账，入口自行决定提示/计数）
	PickupPasswordWrong                        // 密码错误（已在统一键记账，达阈值触发锁定）
)

// PickupDecision CheckPickup 的完整判定。
type PickupDecision struct {
	Verdict       PickupVerdict
	BlockedStatus string // Verdict==PickupBlocked 时有效
	LockRemain    int    // Verdict==PickupLocked 时有效（秒）
}

// PickupCheck 判定入参（HTTP 关注点由适配层提取后传入）。
type PickupCheck struct {
	ClientIP      string // 可信代理解析后的取件方 IP（锁定键维度）
	Code          string // 取件入口使用的码（分享码或取件码，锁定键维度）
	Password      string
	AuthedByToken bool // 持有效下载令牌视为已认证，跳过密码段（share/download 通道）
	// MissingCountsFailure 空密码是否按失败记账：preview/anonymous/download
	// 为 true（对齐各自历史行为），select 为 false（空密码是"需要密码"提示态）。
	MissingCountsFailure bool
}

// CheckPickup 统一取件闸门。判定顺序固定：过期 → 管控 → 就绪 → 锁定 →
// 密码；RequireAuth=false 或 AuthedByToken=true 时密码段整体跳过。
// 密码正确即在统一键清零失败计数；密码错误（含脏数据空哈希）记账一次。
func CheckPickup(ctx context.Context, st PickupState, req PickupCheck) PickupDecision {
	switch {
	case st.Expired:
		return PickupDecision{Verdict: PickupUnavailable}
	case st.Blocked:
		return PickupDecision{Verdict: PickupBlocked, BlockedStatus: st.BlockedStatus}
	case st.NotReady:
		return PickupDecision{Verdict: PickupNotReady}
	}
	lock := middleware.GetDefaultLockout()
	// 枚举维度先判（2026-11-06）：换码也逃不掉。位置刻意排在"是否需要密码"
	// 之前——枚举者命中的往往是**无密码**分享，若把这道闸放在密码段里，
	// 无密码分享的枚举会整条绕过去。
	if enumRemain, enumLocked := PickupEnumLockedRemain(ctx, req.ClientIP); enumLocked {
		return PickupDecision{Verdict: PickupLocked, LockRemain: enumRemain}
	}
	if !st.RequireAuth || req.AuthedByToken {
		return PickupDecision{Verdict: PickupAllowed}
	}

	lockKey := PickupLockKey(req.ClientIP, req.Code)
	if remain, locked := lock.CheckLocked(ctx, lockKey); locked {
		return PickupDecision{Verdict: PickupLocked, LockRemain: remain}
	}
	if req.Password == "" {
		if !req.MissingCountsFailure {
			return PickupDecision{Verdict: PickupPasswordMissing}
		}
		_, _ = lock.RecordFailure(ctx, lockKey)
		return PickupDecision{Verdict: PickupPasswordWrong}
	}
	// 防御历史脏数据：require_auth=true 但哈希缺失 → 一律按密码错误拒绝记账，
	// 而非以"密码错误"之外的方式放行（CheckPassword 对空哈希已收紧为全拒）。
	if st.PasswordHash == "" || !utils.CheckPassword(st.PasswordHash, req.Password) {
		_, _ = lock.RecordFailure(ctx, lockKey)
		return PickupDecision{Verdict: PickupPasswordWrong}
	}
	lock.Reset(ctx, lockKey)
	// 成功也清枚举计数：走到了这里说明凭据是真的（不是猜出来的），
	// 留着它只会让「事后正常取件」被历史误输连坐。
	if req.ClientIP != "" {
		lock.Reset(ctx, PickupEnumLockKey(req.ClientIP))
	}
	return PickupDecision{Verdict: PickupAllowed}
}
