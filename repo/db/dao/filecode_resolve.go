package dao

import (
	"context"
	"strings"

	"github.com/pigeonbox/core/repo/db/model"
)

// ==================== 按码解析分享记录（全站唯一真相源） ====================
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
// 铸造与建码的竞态窗口都会踩到）。本文件把优先级收敛成一处，四处共用。
//
// 优先级（拍板）：**6 位取件码优先**（取件码真相源 = file_codes.pickup_code 列，
// 大小写归一为 UPPER，与铸造/写入侧同口径），否则按分享码/自定义码（精确 →
// 大小写折叠 UPPER 兜底）。
//
// 为什么是"取件码优先"：匿名域与 MCP **本来就是这个优先级**（既有行为）⇒ 对齐到
// 它们等于不改动既有流程；反向改（分享域优先）反而会让分享域与另两处分叉。
//
// 边界：本方法只负责「码 → 记录」。真实状态（过期/管控/密码/次数）由各域拿到
// 记录后自行判定（分享域走 app/share 的 validateShareRecord，匿名域走
// app/anonymous 的 Peek/Retrieve 闸门）。
//
// ⚠️ 2026-10-24：本解析器此前住在 `pkg/codex`，但它 import 了 repo/db/model ⇒
// 违反架构守卫规则 1（pkg 层不得依赖上层）⇒ 下沉到 dao 层（数据访问策略本就属于
// 这一层），pkg 保持「零上层依赖」。判定式 IsNotReady 一并迁到 model 包。

// ResolveByCode 按用户给的码解析分享记录。
//
// ① 6 位输入先查取件码列（file_codes.pickup_code，真相源；UPPER 归一，与铸造侧
//
//	写入的形态一致 ⇒ 取件码大小写不敏感）；
//
// ② 否则查分享码列（精确 → 大小写折叠）。
//
// 查不到时返回**最后一次**的底层错误（调用方据此映射业务码）。
//
// foldEnabled 由调用方传入（配置在 conf 层，数据层不读全局配置）。
func (r *FileCodeRepository) ResolveByCode(ctx context.Context, code string, foldEnabled bool) (*model.FileCode, error) {
	trimmed := strings.TrimSpace(code)

	// ① 取件码优先（仅 6 位输入；其余长度不可能是取件码，省一次索引查询）
	if len(trimmed) == model.PickupCodeLength {
		if fc, err := r.GetByPickupCode(ctx, strings.ToUpper(trimmed)); err == nil && fc != nil {
			return fc, nil
		}
	}

	// ② 分享码 / 自定义码：精确 → 大小写折叠
	//    用 trim 后的串（与取件码分支同口径）：旧实现里 MCP 的 resolveFileCode
	//    会先 TrimSpace 再查 ⇒ 保留该行为，否则 " abc12345 " 这类输入会
	//    突然查不到（且 6 位能解析、8 位不能 ⇒ 自造口径分叉）。
	fc, err := r.GetByCode(ctx, trimmed)
	if err == nil && fc != nil {
		return fc, nil
	}
	if foldEnabled {
		// ⚠️ 2026-10-26 审计修复：原实现这里传的是**未 trim** 的 `code`
		// ⇒ 带首尾空白的输入（" abc12345 "）在精确分支能命中（用 trimmed）、
		// 在折叠分支却永远匹配不上 ⇒ 同一输入两条路径结论相反，与本函数注释
		// 自述的「用 trim 后的串（与取件码分支同口径）」相反。
		if fc2, err2 := r.GetByCodeFolded(ctx, trimmed); err2 == nil && fc2 != nil {
			return fc2, nil
		}
	}
	return nil, err
}
