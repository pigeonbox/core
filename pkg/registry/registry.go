// Package registry 联邦注册中心（p2pd）基址归一——联邦域的单源。
//
// 背景（2026-10-22）：注册中心基址归一此前在 app/p2px（p2px.go/watcher.go）与
// app/federation（directory.go）各存一份**同值私有实现**（连官方公共注册中心
// 常量也是两份副本）——改一处即分叉，且无门禁能发现。本包是该事实的唯一真相
// 源，重复实现已删除。
//
// 2026-10-22 Wave B'：直传编排层（app/p2px）整体退役，消费方只剩联邦域
// （app/federation 的注册/心跳与名录）——本包随之成为联邦域独占的归一实现。
//
// 语义：Effective = 多主备取首个 → 去首尾空白 → 留空回填官方公共注册中心。
// 多主备（逗号分隔）是联邦注册中心的既有形态（conf.FederationConfig.RegistryURL
// 支持 "主,备"，写路径全推、读路径依次 failover）。
//
// 与 pkg/baseurl 同款定位：pkg 是最底层共享库，app 域单向依赖它，反向禁止。
package registry

import "strings"

// DefaultPublic 官方公共注册中心（留空配置的缺省回填目标）。
//
// 直传（p2pc 信箱 watcher/参数）与联邦（注册/心跳/名录）共用同一注册中心。
const DefaultPublic = "http://home.enjoye.top:56788"

// Primary 主备列表取首个非空项（联邦注册中心支持逗号分隔多主备）。
// 全空返回空串（交 Effective 回填缺省）。
func Primary(list string) string {
	for _, one := range strings.Split(list, ",") {
		if one = strings.TrimSpace(one); one != "" {
			return one
		}
	}
	return ""
}

// Effective 注册中心基址归一：多主备取首个 → 去首尾空白 → 留空=官方公共注册中心。
func Effective(u string) string {
	u = Primary(u)
	if u == "" {
		return DefaultPublic
	}
	return u
}
