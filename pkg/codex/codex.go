// Package codex 是「按码解析分享记录」的**唯一真相源**。
//
// 背景（2026-10-23）：取件码（6 位）也能当页面链接用之后，「用户给一串码 ⇒ 哪个
// 分享」这件事被**四个域**各写了一遍，且**优先级相反**：
//
//	分享域（app/share.GetFileByCode）   分享码优先 → 取件码兜底
//	预览域（app/preview）               只查分享码列（不认取件码）
//	匿名域（app/anonymous）             取件码优先 → 分享码
//	MCP（app/mcp.resolveFileCode）      取件码优先 → 分享码（且不折叠大小写）
//
// ⇒ 同一串码在不同端点可能解析到**不同分享**（历史脏数据/大小写折叠变体/
// 铸造与建码的竞态窗口都会踩到）。本包把优先级收敛成一处，四处共用。
//
// 优先级（拍板）：**6 位取件码优先**（取件码真相源 = file_codes.pickup_code 列，
// 大小写归一为 UPPER，与铸造/写入侧同口径），否则按分享码/自定义码（精确 →
// 大小写折叠 UPPER 兜底）。
//
// 为什么是"取件码优先"：匿名域与 MCP **本来就是这个优先级**（既有行为）⇒ 对齐到
// 它们等于不改动既有流程；反向改（分享域优先）反而会让分享域与另两处分叉。
//
// 边界：本包只负责「码 → 记录」。真实状态（过期/管控/密码/次数）由各域拿到
// 记录后自行判定（分享域走 app/share 的 validateShareRecord，匿名域走
// app/anonymous 的 Peek/Retrieve 闸门）。
package codex

import (
	"context"
	"strings"

	"github.com/pigeonbox/core/repo/db/model"
)

// PickupCodeLength 取件码长度（6 位）——**唯一真相源**。
//
// 此前三处各写一份（app/anonymous 的 codeLength、app/share 的 pickupCodeLength、
// app/mcp 的字面量 6）⇒ 任一处改长度即静默漂移（分享域兜底失效 / MCP 解析失效）。
const PickupCodeLength = 6

// Repo 是按码解析所需的最小数据访问面（窄接口：便于单测注入假实现，
// 也让本包不必依赖具体 dao 实现）。
type Repo interface {
	GetByCode(ctx context.Context, code string) (*model.FileCode, error)
	GetByCodeFolded(ctx context.Context, code string) (*model.FileCode, error)
	GetByPickupCode(ctx context.Context, pickupCode string) (*model.FileCode, error)
}

// Resolve 按用户给的码解析分享记录。
//
// ① 6 位输入先查取件码列（file_codes.pickup_code，真相源；UPPER 归一，与铸造侧
//    写入的形态一致 ⇒ 取件码大小写不敏感）；
// ② 否则查分享码列（精确 → 大小写折叠 UPPER 兜底，折叠开关由调用方传入 ——
//    本包不读全局配置，保持"数据层不依赖配置"的纪律）。
//
// 查不到时返回**最后一次**的底层错误（调用方据此映射业务码）。
func Resolve(ctx context.Context, repo Repo, code string, foldEnabled bool) (*model.FileCode, error) {
	trimmed := strings.TrimSpace(code)

	// ① 取件码优先（仅 6 位输入；其余长度不可能是取件码，省一次索引查询）
	if len(trimmed) == PickupCodeLength {
		if fc, err := repo.GetByPickupCode(ctx, strings.ToUpper(trimmed)); err == nil && fc != nil {
			return fc, nil
		}
	}

	// ② 分享码 / 自定义码：精确 → 大小写折叠
	fc, err := repo.GetByCode(ctx, code)
	if err == nil && fc != nil {
		return fc, nil
	}
	if foldEnabled {
		if fc2, err2 := repo.GetByCodeFolded(ctx, code); err2 == nil && fc2 != nil {
			return fc2, nil
		}
	}
	return nil, err
}
