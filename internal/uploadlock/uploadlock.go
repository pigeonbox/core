// Package uploadlock 同一 upload_id 完成面的**排队互斥**（core#151）。
//
// 缺陷背景：presign/chunk 两条上传完成路径的幂等都是 read-then-create——
// 「读完成状态 → 建分享 → 标记完成」之间无锁，DB 的 file_codes.upload_id 列
// 也没有唯一约束兜底 ⇒ 并发两次 Complete（客户端超时重试 / 双击 / HTTP 层
// 重放）各自读到「未完成」⇒ 同一 upload_id 建出 2 条分享、回 2 个取件码，
// 幂等形同虚设；chunk 路径还会各自 Merge 出一份 UUID 物理文件（磁盘双份）。
//
// 语义：同 uploadID 的完成面**排队**——后到者等前一个跑完再读状态，读到的
// 必然是「已完成」⇒ 走各自既有的幂等回读分支返回**同一条**分享。同属主重复
// Complete 仍幂等返回同一条（与串行两次调用的行为逐字一致，只消灭并发窗口）。
//
// 为什么条纹化（fnv32 → 固定 64 槽）：底座 KeyedGate 的键集契约是「调用方
// 保证有界」（slots 只增不减、无回收），而 uploadID 是高基数（每上传会话一个）
// ⇒ 直接当键会慢性泄漏；哈希进固定槽位后键集恒 ≤ 64。同槽位的不同 uploadID
// 偶发排队只是延迟，不影响互斥正确性。
//
// 为什么不换 KeyedMutex（oekit 同包近邻）：它是**即拒**语义（key 忙立即
// ok=false），完成面要的却是排队——即拒会让并发重试拿到错误而不是同一条分享
// （把幂等语义改坏）。
//
// 覆盖面：进程内互斥（单机 standalone 的全部形态，以及多副本各自的同刻并发）。
// 跨副本（deployment.mode=public×N）同刻撞同一 uploadID 的窗口不在此原语
// 覆盖内——彻底收口需带属主的条件唯一索引（upload_id 可由 file_hash 派生，
// 不同属主持同一 uploadID 是合法形态）+ 存量重复行迁移，见 core#151 PR 说明。
package uploadlock

import (
	"context"
	"hash/fnv"
	"strconv"

	"github.com/oekit/concurrency/syncx"
)

// stripes 条纹数：键集上界（KeyedGate slots ≤ stripes）。64 对完成面的并发
// 量级（同刻至多几个 Complete）足够打散，碰撞只造成偶发排队。
const stripes = 64

// gate 每槽 cap=1 的排队闸门：同槽同刻至多一个完成面在跑。
var gate = syncx.NewKeyedGate(1)

// Lock 排队取得 uploadID 的完成面互斥权；返回幂等 release（调用方 defer）。
// ctx 取消/超时返回 ctx.Err()——此时未做任何事，调用方按失败返回即可。
func Lock(ctx context.Context, uploadID string) (release func(), err error) {
	return gate.Acquire(ctx, stripeKey(uploadID))
}

// stripeKey uploadID → 固定槽位键（"0".."63"）。fnv32 无种子、确定性映射，
// 同一 uploadID 恒落同槽。
func stripeKey(uploadID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(uploadID))
	return strconv.FormatUint(uint64(h.Sum32()%stripes), 10)
}
