package config

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pigeonbox/core/pkg/logger"
)

// UpdateConfigJSON 按**字段级**合并提交的 JSON 到已落库配置后保存。
//
// 此前 PUT /admin/config 的语义只到「段级」：nil 段保留现状，但**段内**未提及的
// 字段会被**整体清零**（JSON 反序列化进结构体 ⇒ 未提及字段取零值）。实测后果
// （2026-10-23 真机）：
//
//	PUT {"upload_ex":{"max_file_size":1024}}
//	  ⇒ upload_ex 其余字段全部归零 ⇒ open_upload=false（匿名上传被"关闭"，
//	    /share/file/ 立刻 10012）、enable_chunk=false、enable_magic_check=false、
//	    text_max_bytes=0、local_import.roots 清空……
//	  ⇒ 看起来只改了一个字段，实际把整段配置弄没了。
//
// 新语义（**字段级合并**）：
//   - 提交里**出现**的字段生效（显式 null 视为"清空该字段" ⇒ **切片/指针项
//     可清空**；⚠️ **标量字段的 null 是 no-op** —— Go 的 json 把 null 解进
//     非指针标量不改值（那些字段的 0 本身就是"不限/关闭"语义，清不清等价）；
//   - 提交里**未出现**的字段保留现状（含嵌套对象：改 notify.smtp.host 不会把
//     同级的 smtp.port/smtp.username 清零）；
//   - 未提交的段整体保留。
//
// 合并底座用**已落库**的配置（不是 overlay 后的运行态：overlay 含 env 种子，
// 把它当底座会把 env 值固化进库）。
func (s *Service) UpdateConfigJSON(ctx context.Context, raw []byte) error {
	// ① 提交体必须是 JSON 对象（段集合）
	var incoming map[string]json.RawMessage
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return fmt.Errorf("配置体必须是 JSON 对象: %w", err)
	}

	// ①.5 形状归一：local_import 有**两种提交形状**——顶层 local_import 与
	// upload_ex 内嵌的 local_import（读侧两种都吐；官方管理台用顶层）。
	//
	// 缺陷实录（2026-10-30 真机实测）：此前只认顶层 ⇒ 只提交**嵌套**形状的
	// 集成方**静默无效但返回 success**——回读仍是旧值，调用方以为改成功了。
	//   PUT {"upload_ex":{"local_import":{"enabled":true,"roots":["/x"]}}}
	//     ⇒ {"code":0,"message":"success"}，但 roots 纹丝不动。
	// ⇒ 本次提交**未带**顶层形状时，把嵌套形状提升为顶层（提交意图以本次请求为准）；
	// 两者都在则顶层为准（与运行时取值口径一致：sc.LocalImport 覆盖 sc.UploadEx）。
	normalizeLocalImportShape(incoming)

	// ② 底座 = 已落库配置（无记录 ⇒ 底座为空 ⇒ 语义退化为"整段替换"，
	//    与旧行为一致）
	base := map[string]json.RawMessage{}
	persisted, err := s.loadPersistedConfig(ctx)
	if err != nil {
		return err
	}
	if persisted != nil {
		b, mErr := json.Marshal(persisted)
		if mErr != nil {
			return mErr
		}
		if mErr = json.Unmarshal(b, &base); mErr != nil {
			return mErr
		}
	} else {
		logger.CtxWarn(ctx, "system config persisted record absent; incoming json replaces whole config")
	}

	// ③ 字段级合并（incoming 覆盖 base；两侧都是对象时递归；显式 null 覆盖）
	merged, err := json.Marshal(mergeJSONObjects(base, incoming))
	if err != nil {
		return err
	}

	// ④ 走既有通道：类型化解码 → 校验 → 落库 → 热应用（副作用与钩子都在
	//    UpdateConfig 内 ⇒ 本方法不重复实现任何副作用）
	cfg := &SystemConfig{}
	if err := json.Unmarshal(merged, cfg); err != nil {
		return err
	}
	return s.UpdateConfig(ctx, cfg)
}

// normalizeLocalImportShape 把 upload_ex 内嵌的 local_import 提升为顶层形状。
//
// 仅当本次提交**未带**顶层 local_import 时生效（两者都在 ⇒ 顶层为准，
// 与运行时的取值口径一致：sc.LocalImport 覆盖 sc.UploadEx.LocalImport）。
//
// 背景：local_import 同一配置有两种提交形状（顶层 / upload_ex 内嵌），
// 读侧两种都吐；此前写侧只认顶层 ⇒ 嵌套形状**静默无效但返回 success**。
func normalizeLocalImportShape(incoming map[string]json.RawMessage) {
	if _, hasTop := incoming["local_import"]; hasTop {
		return
	}
	ue, ok := incoming["upload_ex"]
	if !ok {
		return
	}
	var uploadEx map[string]json.RawMessage
	if err := json.Unmarshal(ue, &uploadEx); err != nil {
		return
	}
	li, ok := uploadEx["local_import"]
	if !ok || string(li) == "null" {
		return
	}
	incoming["local_import"] = li
}

// mergeJSONObjects 按字段级合并两个 JSON 对象：
//   - incoming 出现的键覆盖 base 的同名键；
//   - 显式 null（"key": null）覆盖 base ⇒ 表示"清空该字段"；
//   - 两侧都是 JSON 对象时**递归**合并（否则嵌套对象会整体替换 ⇒
//     例如改 notify.smtp.host 会把 smtp.port/username 清零）；
//   - 其余类型（数组/标量/null）取 incoming。
func mergeJSONObjects(base, incoming map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(base)+len(incoming))
	for k, v := range base {
		out[k] = v
	}
	for k, iv := range incoming {
		bv, ok := out[k]
		if !ok {
			out[k] = iv
			continue
		}
		var bmap, imap map[string]json.RawMessage
		bIsObj := json.Unmarshal(bv, &bmap) == nil && bmap != nil
		iIsObj := json.Unmarshal(iv, &imap) == nil && imap != nil
		if bIsObj && iIsObj {
			merged, err := json.Marshal(mergeJSONObjects(bmap, imap))
			if err != nil {
				// 合并失败（理论上不可达：两侧已确为对象）⇒ 取 incoming，
				// 不静默丢字段
				out[k] = iv
				continue
			}
			out[k] = merged
			continue
		}
		out[k] = iv
	}
	return out
}
