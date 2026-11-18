// 按目录的分享策略层（core#159 地基）：路径前缀匹配 + 分享模式闸口。
//
// 定位：目录策略是**站点/用户配额账本之上的策略层**（按路径前缀分组限额/开关），
// 不改 reserveUploadCaps 的原子预留账本（core#196/#169 的地盘）。
//
// 当前接线面：本地导入（ImportLocalFile）——NAS 场景里目录概念唯一可归因的
// 入口（分享行本身不记录来源目录）。通用上传通道（直传/分片/预签名/多文件）
// 没有「来源目录」概念 ⇒ 不参与目录策略与目录配额。
//
// quota_bytes 已接线（core#159）：本地导入命中某条策略时，用量按
// file_codes.dir_root 归因到该策略的 root，判定在**落盘前**做（见
// app/share/quota_reserve.go 的 reserveDirectoryQuota）。
//
// internal_only 已接线（core#233）：语义**两环都约束**——
// ① 生成环：不铸造 6 位公网取件码（收口在 app/share/service.go 的 CreateShare）；
// ② 取件环：内容交付面强制登录（闸口在 pkg/gate.CheckInternalOnlyLogin）。
// ⇒ 创建面（这里）**放行** internal_only：它不是「不许分享」，而是「分享仅站内可取」。
//
// 判定基准：命中与否在**创建时**定型，标记落在分享行上
// （model.FileCode.InternalOnly，core#233）⇒ 取件热路径零 I/O、零 fail-open；
// 与 deny 同款口径（只约束创建，不追溯存量）。
package share

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pigeonbox/core/conf"
)

// matchDirectoryPolicy 按目录前缀匹配策略：返回命中的最长前缀条目，无命中返回 nil。
//
// 语义（测试钉住）：
//   - 分隔符感知：/data/music 匹配 /data/music 与 /data/music/x，
//     不匹配 /data/music2（前缀字符串匹配的经典边界）；
//   - 最长前缀优先：/a 与 /a/b 同时命中 /a/b/c 时取 /a/b，与配置顺序无关；
//   - policy.Root 非绝对路径 / 空 ⇒ 跳过（匹配不到任何绝对路径——配置必须用
//     绝对路径，管理台校验后续补，见 conf 注释 TODO）；
//   - absPath 先 Clean，比较在平台分隔符语义下进行。
func matchDirectoryPolicy(policies []conf.DirectoryPolicyConfig, absPath string) *conf.DirectoryPolicyConfig {
	if len(policies) == 0 {
		return nil
	}
	p := filepath.Clean(absPath)
	sep := filepath.Separator
	best := -1
	var matched *conf.DirectoryPolicyConfig
	for i := range policies {
		root := filepath.Clean(strings.TrimSpace(policies[i].Root))
		if root == "" || root == "." || !filepath.IsAbs(root) {
			continue
		}
		hit := p == root ||
			(strings.HasPrefix(p, root) &&
				(strings.HasSuffix(root, string(sep)) || p[len(root)] == byte(sep)))
		if hit && len(root) > best {
			best = len(root)
			matched = &policies[i]
		}
	}
	return matched
}

// resolveDirectoryPolicies 把策略 root 解析为真实路径（EvalSymlinks）。
//
// 必须解析：调用方传入的 absPath 已经过 EvalSymlinks（如 macOS 上
// /var/folders → /private/var/folders），策略 root 不解析就会**永不匹配**
// （静默失效——策略面最典型的坏法）。口径与 withinRoots 逐 root 解析一致：
// 解析失败（目录不存在/不可达）跳过；非绝对路径在 EvalSymlinks 前挡住
// （相对路径会按 CWD 解析成错误的绝对路径）。
func resolveDirectoryPolicies(policies []conf.DirectoryPolicyConfig) []conf.DirectoryPolicyConfig {
	if len(policies) == 0 {
		return nil
	}
	out := make([]conf.DirectoryPolicyConfig, 0, len(policies))
	for _, p := range policies {
		root := filepath.Clean(strings.TrimSpace(p.Root))
		if root == "" || root == "." || !filepath.IsAbs(root) {
			continue
		}
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		p.Root = real
		out = append(out, p)
	}
	return out
}

// resolveRealPath 把待判定路径解析为真实路径；解析失败（路径尚不存在等）
// 回退 Clean——入口两侧同口径解析，调用方传未解析路径也不会静默不命中。
func resolveRealPath(p string) string {
	c := filepath.Clean(p)
	if real, err := filepath.EvalSymlinks(c); err == nil {
		return real
	}
	return c
}

// resolveDirectoryPolicyMatch 解析并匹配命中的策略条目：两侧同口径解析
// （策略 root 与待判定路径都走 EvalSymlinks）⇒ 调用方不会漏解析。
//
// 返回的条目里的 Root 是**解析后**的形态 ⇒ 它同时是：
//   - 目录配额用量归因的 key（file_codes.dir_root 落的就是它 ⇒ 同一策略的
//     多次导入聚合到同一个目录，软链目录不分裂成两份用量）；
//   - 错误文案里点名的目录（运维能直接定位）。
func resolveDirectoryPolicyMatch(policies []conf.DirectoryPolicyConfig, absPath string) *conf.DirectoryPolicyConfig {
	return matchDirectoryPolicy(resolveDirectoryPolicies(policies), resolveRealPath(absPath))
}

// checkDirectoryPolicyMatch 目录策略闸口（已匹配条目）：nil = 放行；非 nil =
// 拒绝（错误文案定位到目录）。
//
// fail-closed：命中策略但 share_mode 是未知取值（拼错/未实现的新模式）⇒ 拒绝
// 并点名配置错误——策略面宁可挡住也不静默放行（与审核 fail-closed 同哲学）。
//
// internal_only（core#233）**放行创建**：它是「仅站内可取」而不是「不许分享」；
// 其约束落在铸码（生成环）与取件（取件环）两处，不在创建面。
func checkDirectoryPolicyMatch(p *conf.DirectoryPolicyConfig) error {
	if p == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(p.ShareMode)) {
	case "", conf.ShareModeAllow:
		return nil
	case conf.ShareModeDeny:
		return fmt.Errorf("该目录已禁止生成分享（目录策略 root=%s）", p.Root)
	case conf.ShareModeInternalOnly:
		// 仅站内：允许创建，但不出公网取件码 + 取件强制登录（core#233）。
		return nil
	default:
		return fmt.Errorf("目录策略 root=%s 的 share_mode 非法: %q（只允许 allow/deny/internal_only）", p.Root, p.ShareMode)
	}
}

// checkDirectoryPolicy 目录策略闸口：nil = 放行；非 nil = 拒绝（错误文案定位到目录）。
// 入口即解析（策略 root 与待判定路径都走 EvalSymlinks），调用方不会漏解析。
func checkDirectoryPolicy(policies []conf.DirectoryPolicyConfig, absPath string) error {
	return checkDirectoryPolicyMatch(resolveDirectoryPolicyMatch(policies, absPath))
}
