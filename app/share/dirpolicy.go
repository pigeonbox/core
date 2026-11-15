// 按目录的分享策略层（core#159 地基）：路径前缀匹配 + 分享模式闸口。
//
// 定位：目录策略是**站点/用户配额账本之上的策略层**（按路径前缀分组限额/开关），
// 不改 reserveUploadCaps 的原子预留账本（core#196/#169 的地盘）。
//
// 当前接线面：本地导入（ImportLocalFile）——NAS 场景里目录概念唯一可归因的
// 入口（分享行本身不记录来源目录）。通用上传通道的「分享路径前缀」维度与
// quota_bytes 用量归因见 conf.DirectoryPolicyConfig 的 TODO。
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

// checkDirectoryPolicy 目录策略闸口：nil = 放行；非 nil = 拒绝（错误文案定位到目录）。
// 入口即解析（策略 root 与待判定路径都走 EvalSymlinks），调用方不会漏解析。
//
// fail-closed：命中策略但 share_mode 是未知取值（拼错/未实现的新模式）⇒ 拒绝
// 并点名配置错误——策略面宁可挡住也不静默放行（与审核 fail-closed 同哲学）。
func checkDirectoryPolicy(policies []conf.DirectoryPolicyConfig, absPath string) error {
	p := matchDirectoryPolicy(resolveDirectoryPolicies(policies), resolveRealPath(absPath))
	if p == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(p.ShareMode)) {
	case "", "allow":
		return nil
	case "deny":
		return fmt.Errorf("该目录已禁止生成分享（目录策略 root=%s）", p.Root)
	default:
		return fmt.Errorf("目录策略 root=%s 的 share_mode 非法: %q（只允许 allow/deny）", p.Root, p.ShareMode)
	}
}
