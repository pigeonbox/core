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
//   - 提交里**出现**的字段生效（显式 null 视为"清空该字段" ⇒ 可清列表/指针项）；
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
