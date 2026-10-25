// presign_policy_migration.go — 旧直传开关 download.presign_anonymous_enabled
// 退役（2026-10-26 兼容层清理）。
//
// 背景：直传策略先后经历两代形态：
//
//	① 旧：download.presign_anonymous_enabled（bool，true=匿名可直传）
//	② 新：download.presign_policy（三档 everyone/authenticated/disabled）
//
// ② 引入时 ① 被保留为回退读路径（显式 ② 为空 ⇒ 读 ①）。本次退役 ① 时，
// 存量库里仍可能带着 ① 的值：**直接忽略会让「曾关闭匿名直传」的站点
// 静默变成 everyone**（= 桶沦为免费匿名网盘，见 presign handler 的动机注释）。
//
// 处置：**读取时并入 ② + 启动响亮提示**。
//   - ② 已显式设置 ⇒ 以 ② 为准，① 直接忽略（管理员已在管理台改过策略）。
//   - ② 未设置（空串）⇒ 由 ① 推导：false→authenticated，true→everyone。
//   - ① 从数据库消失：管理台整段保存即自然清掉（json 解码忽略未知键，
//     重新序列化时不带该键）。
package config

import (
	"encoding/json"
	"strings"

	"go.uber.org/zap"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/logger"
)

// legacyPresignAnonymous 检出的旧直传开关。
type legacyPresignAnonymous struct {
	// Enabled 旧开关的值（true=允许匿名直传）。
	Enabled bool
	// Raw 该键的原始 JSON（用于日志里如实呈现，null 表示显式置空）。
	Raw json.RawMessage
}

// detectLegacyPresignAnonymous 从持久化 JSON **原文**检出旧开关。
//
// 为什么查原文而不查解码后的结构体：旧字段已从 conf.DownloadConfig 删除，
// 解码后根本不存在该字段 —— 只有原文能证明「DB 里曾经有它」。
//
// 返回 ok=false = 未检出（无 download 段 / 无该键 / 原文不是合法 JSON）。
func detectLegacyPresignAnonymous(raw string) (legacyPresignAnonymous, bool) {
	var probe struct {
		Download map[string]json.RawMessage `json:"download"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return legacyPresignAnonymous{}, false
	}
	if len(probe.Download) == 0 {
		return legacyPresignAnonymous{}, false
	}
	rawVal, ok := probe.Download["presign_anonymous_enabled"]
	if !ok {
		return legacyPresignAnonymous{}, false
	}
	out := legacyPresignAnonymous{Raw: rawVal}
	var b bool
	if err := json.Unmarshal(rawVal, &b); err == nil {
		out.Enabled = b
	}
	return out, true
}

// describeLegacyPresignSwitch 旧开关的可读描述（日志用）。
func describeLegacyPresignSwitch(v legacyPresignAnonymous) string {
	if len(v.Raw) == 0 {
		return "null"
	}
	return strings.TrimSpace(string(v.Raw))
}

// applyLegacyPresignPolicy 把旧开关语义并入 presign_policy。
//
// 规则：② 已显式设置 ⇒ 以 ② 为准；② 未设置 ⇒ 由 ① 推导（false→authenticated，
// true→everyone）。旧键本身不写入结构体（字段已退役）⇒ 管理台整段保存即从库中
// 消失。
func applyLegacyPresignPolicy(cfg *SystemConfig, raw string) {
	if cfg == nil {
		return
	}
	legacy, ok := detectLegacyPresignAnonymous(raw)
	if !ok {
		return
	}
	if cfg.Download == nil {
		cfg.Download = &conf.DownloadConfig{}
	}
	if cfg.Download.PresignPolicy == "" {
		derived := conf.PresignPolicyEveryone
		if !legacy.Enabled {
			derived = conf.PresignPolicyAuthenticated
		}
		cfg.Download.PresignPolicy = derived
		logger.Warn("检测到陈旧配置 download.presign_anonymous_enabled，已并入 download.presign_policy",
			zap.String("legacy_value", describeLegacyPresignSwitch(legacy)),
			zap.String("derived_policy", derived))
		return
	}
	logger.Warn("检测到陈旧配置 download.presign_anonymous_enabled，已忽略（download.presign_policy 已显式设置）",
		zap.String("legacy_value", describeLegacyPresignSwitch(legacy)),
		zap.String("policy", cfg.Download.PresignPolicy))
}
