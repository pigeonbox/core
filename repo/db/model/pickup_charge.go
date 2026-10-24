package model

import (
	"time"

	"gorm.io/gorm"
)

// PickupCharge 取件计次记录：**一个下载令牌（取件会话）至多扣 1 次**。
//
// 为什么需要这张表（2026-10-24）：
// 「取件次数」的语义是「允许被取走多少次」，不是「允许发多少个 HTTP 请求」。
// 此前扣次点在**每次下载请求**上 ⇒ 三类正常行为都会重复吃次数：
//   - 多文件分享逐文件下载（前端每个文件一个请求）⇒ 取件次数=1 的多文件分享
//     只能下载**第一个**文件（第二个 403「取件次数已用完」），而「打包下载」
//     同一个 zip 只扣 1 次 ⇒ 两条 UI 路径的次数语义不可比；
//   - 断点续传 / 下载器重试（Range 请求各自是一次完整鉴权）⇒ 一次逻辑下载
//     吃 N 次次数；
//   - 收件人重复点击下载 ⇒ 同样重复吃次数。
//
// 计次键 = **下载令牌的指纹**（令牌即「一次取件」的凭证）：
// 同一令牌 = 同一次取件会话 ⇒ 只扣 1 次；不同令牌（重新取件）= 不同取件人/次
// ⇒ 各自扣 1 次。指纹用令牌签名段（HMAC 输出，不可伪造、不泄露口令）。
//
// 真相源是 DB（与项目纪律一致：不依赖 Redis ⇒ 单机内存模式与多副本部署行为一致）。
type PickupCharge struct {
	gorm.Model
	// Code 规范分享码（8 位 file_codes.code；取件码入口解析后写入）。
	Code string `gorm:"size:255;not null;uniqueIndex:idx_pickup_charge_code_token" json:"code"`
	// TokenHash 令牌指纹（令牌签名段，hex）。
	TokenHash string `gorm:"size:64;not null;uniqueIndex:idx_pickup_charge_code_token" json:"token_hash"`
	// Channel 计次来源通道（select / retrieve / anonymous-download），仅供观测。
	Channel string `gorm:"size:32" json:"channel"`
	// ChargedAt 计次时刻（= 计次成功时间）。
	ChargedAt time.Time `gorm:"index" json:"charged_at"`
}

// TableName 固定表名。
func (PickupCharge) TableName() string { return "pickup_charges" }
