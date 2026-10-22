// stale.go — 陈旧配置键检出（2026-10-22 Wave B'：设备直传面退役）。
//
// 背景：Wave B' 从 SystemConfig.p2p 段删掉了 direct 子段（p2pc 直传参数）。
// Go 的 json 解码**静默忽略**未知键 —— 存量库里若仍有
// `{"p2p":{"direct":{"registry_url":"http://p2p:56788",...}}}`，该段会被
// 无声丢弃：用户以为「注册中心还配着」，实际联邦名录已失去在线对端
// （名录的 registry 真相源只剩 p2p.federation.registry_url）。
//
// 处置：**忽略 + 启动响亮提示**。本文件只做「检出」，不修复、不删库
// （改库是管理员的动作，且 admin 后台整段保存即自然清掉该键）。
package config

import (
	"encoding/json"
	"sort"
	"strings"
)

// staleP2PDirect 检出的陈旧 p2p.direct 段。
type staleP2PDirect struct {
	// Keys 段内实际存在的键名（排序后；如 enabled/registry_url/...）。
	Keys []string
	// RegistryURL 段内 registry_url 的值（空 = 未配或为空串）。
	RegistryURL string
}

// detectStaleP2PDirect 从持久化 JSON **原文**检出陈旧的 p2p.direct 段。
//
// 为什么查原文而不查解码后的结构体：direct 段已从 SystemConfig 删除，
// 解码后根本不存在该字段 —— 只有原文能证明「DB 里曾经有它」。
//
// 返回 ok=false = 未检出（无 p2p 段 / 无 direct 子段 / direct 为空对象 /
// 原文不是合法 JSON）。
func detectStaleP2PDirect(raw string) (staleP2PDirect, bool) {
	var probe struct {
		P2P *struct {
			Direct map[string]json.RawMessage `json:"direct"`
		} `json:"p2p"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return staleP2PDirect{}, false
	}
	if probe.P2P == nil || len(probe.P2P.Direct) == 0 {
		return staleP2PDirect{}, false
	}
	out := staleP2PDirect{Keys: make([]string, 0, len(probe.P2P.Direct))}
	for k := range probe.P2P.Direct {
		out.Keys = append(out.Keys, k)
	}
	sort.Strings(out.Keys)
	if raw, ok := probe.P2P.Direct["registry_url"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			out.RegistryURL = strings.TrimSpace(s)
		}
	}
	return out, true
}

// qualifiedKeys 拼出带前缀的键名清单（p2p.direct.registry_url 形态）。
func (s staleP2PDirect) qualifiedKeys() []string {
	out := make([]string, 0, len(s.Keys))
	for _, k := range s.Keys {
		out = append(out, "p2p.direct."+k)
	}
	return out
}
