// Package paircode 配对口令（p2pc 口令）归一——联邦域口令处理的单源。
//
// 背景（2026-10-22）：口令归一此前在 app/p2px（spawn.go）与 app/federation
// （invite.go）各存一份**同值私有实现**——两份副本改一处即分叉，且无任何门禁
// 能发现（编译器不报错、测试各测各的）。本包是该事实的唯一真相源。
//
// 2026-10-22 Wave B'：直传编排层（app/p2px）整体退役，消费方只剩联邦域
// （app/federation 的邀请入箱与推送）——本包随之成为联邦域独占的归一实现。
//
// 形态：12 位字符 + 2 连字符（XXXX-XXXX-XXXX）。归一步骤与搬迁前逐字一致：
// 去首尾空白 → 大写 → 剥连字符 → 恰好 12 位时按 4-4-4 回插连字符
// （p2pc 接受裸串，规整后展示/日志一致）。非 12 位输入原样返回（大写去连字符
// 后），格式校验交调用方（各域的错误文案不同，不在此包收口）。
//
// 与 pkg/baseurl 同款定位：pkg 是最底层共享库，app 域单向依赖它，反向禁止。
package paircode

import "strings"

// Normalize 口令归一（去空白/连字符、大写、12 位按 4-4-4 重新分组）。
//
// 空串返回空串；非 12 位输入不做分组（由调用方判格式）。
func Normalize(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	code = strings.ReplaceAll(code, "-", "")
	if code == "" {
		return ""
	}
	// 4-4-4 分组回连字符（p2pc 接受裸串，规整后展示/日志一致）
	if len(code) == 12 {
		return code[:4] + "-" + code[4:8] + "-" + code[8:]
	}
	return code
}
