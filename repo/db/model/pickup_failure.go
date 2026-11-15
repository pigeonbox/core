package model

import (
	"time"

	"gorm.io/gorm"
)

// 取件失败原因枚举（wire 语义与 errcode 对齐，不泄内部细节）。
//
// frontend-core#93「取件失败面透明化」拍板：失败原因用**错误码语义枚举**，
// 复用错误码文案（20004 次数耗尽 ≠ 404 过期），不落任何内部细节
// （锁定剩余秒数、哈希比对结果、DB 错误串一律不进本表）。
const (
	// PickupFailPasswordWrong 密码错误（wire：20003/401 密码错误语义）。
	PickupFailPasswordWrong = "password_wrong"
	// PickupFailCountExhausted 取件次数耗尽（wire：20004 CodeShareReachLimit/403）。
	PickupFailCountExhausted = "count_exhausted"
	// PickupFailExpired 分享已过期（wire：404 分享不存在或已过期）。
	PickupFailExpired = "expired"
)

// PickupFailure 取件失败事件（frontend-core#93，2026-11-16）。
//
// 为什么单独一张表而不是塞进 pickup_charges：
//   - pickup_charges 的行语义是「**一次成功取件** = 一行」（计次去重键 +
//     owner 取件历史），把失败混进去会直接破坏「一次取件 = 一行」的展示口径；
//   - 失败事件保留 7 天（与分享过期语义对齐），成功明细按
//     admin.pickup_detail_retention_days（默认 30 天）——两张表生命周期不同。
//
// 隐私边界（拍板）：
//   - **不落明文 IP**：只落 SourceHash（sha256(规范分享码|IP) 截断），供
//     「来源计数 / 是否同一来源」两类粗粒度聚合；展示面不下发指纹本身；
//   - **只记录、不扣次**：与「失败不消耗次数」的计次不变量完全正交
//     （docs/specs/2026-10-24-pickup-count-semantics-design.md 17 条不变量不动）。
type PickupFailure struct {
	gorm.Model
	// Code 规范分享码（8 位 file_codes.code；取件码入口解析后写入）。
	// 只有**能归因到某条分享**的失败才落表（码不存在的探测挂不上分享行）。
	Code string `gorm:"size:255;not null;index" json:"code"`
	// Reason 失败原因枚举（PickupFail* 常量）。
	Reason string `gorm:"size:32;not null;index" json:"reason"`
	// SourceHash 粗粒度来源指纹（sha256(码|IP) 前 32 hex；空 = 来源未知）。
	// 与展示面的唯一契约是「可去重计数 + 可判等」——不下发、不可逆回明文 IP。
	SourceHash string `gorm:"size:64" json:"source_hash"`
	// Channel 交付面（select / download），观测用。
	Channel string `gorm:"size:32" json:"channel"`
	// FailedAt 失败时刻（事件时间；CreatedAt 是落库时刻，语义不同故单列）。
	FailedAt time.Time `gorm:"index" json:"failed_at"`
}

// TableName 固定表名。
func (PickupFailure) TableName() string { return "pickup_failures" }
