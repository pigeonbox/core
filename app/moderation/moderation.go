// Package moderation 内容审核钩子（治理 2026-10-03，方案 A：管控优先、审核留钩子）。
//
// 设计：上传/分享入口在写库前调用 Moderator 做内容判定，命中可疑内容的处置
// 由配置决定（直接拒绝 或 建分享后置 pending_review 进管理端队列）。
// 内置 v1 只做文本敏感词；文件侧仅留接口（内置实现恒放行），外接扫描器
// （ClamAV/NSFW 等）以实现 Moderator 接口注入即可，无需改动业务代码。
package moderation

import (
	"context"
	"strings"
	"sync"
)

// Verdict 内容判定结论
type Verdict int

const (
	VerdictAllow   Verdict = iota // 放行
	VerdictReject                 // 拒绝（建分享前拦截）
	VerdictPending                // 待审核（建分享后置 pending_review，取件拒绝）
	// VerdictScanUnavailable 扫描不可用拒收（core#161）：moderation.clamav.strict=true
	// 且 clamd 不可达/超时/内容超 max_scan_bytes ⇒ 无法完成扫描 ⇒ 拒收该次上传。
	// 与 VerdictReject 分开：对外错误文案/码不同（「扫描暂不可用，请稍后重试」
	// ≠ 「内容未通过安全审核」），把「扫描器挂了」误报成「内容违规」会误导排查。
	VerdictScanUnavailable
)

func (v Verdict) String() string {
	switch v {
	case VerdictReject:
		return "reject"
	case VerdictPending:
		return "pending"
	case VerdictScanUnavailable:
		return "scan_unavailable"
	default:
		return "allow"
	}
}

// UploadMeta 文件上传元信息（文件侧钩子入参；路径内容不做读取，由外接扫描器自行处理）
type UploadMeta struct {
	Code        string
	FileName    string
	ContentType string
	Size        int64
	OwnerIP     string
	UserID      *uint
	UploadID    string // 分片会话 ID（分片/秒传通道）
	Channel     string // 上传通道：direct/chunk/presign
	// StoragePath 已落存储的相对路径（文件内容扫描用，如 ClamAV；
	// 直传/绑定入口在审核调用时填充，文本分享为空）
	StoragePath string
}

// Moderator 内容审核接口（业务入口唯一依赖）。
type Moderator interface {
	InspectText(ctx context.Context, text string) Verdict
	InspectFile(ctx context.Context, meta UploadMeta) Verdict
}

// WordListModerator 敏感词表实现（子串匹配，大小写不敏感）。
// Action: "reject"（默认）或 "pending"，决定命中后的处置策略。
type WordListModerator struct {
	mu     sync.RWMutex
	words  []string
	action string // reject | pending
}

// NewWordListModerator 构建敏感词审核器。words 为空时全部放行（功能未启用）。
func NewWordListModerator(words []string, action string) *WordListModerator {
	if action != "pending" {
		action = "reject"
	}
	norm := make([]string, 0, len(words))
	for _, w := range words {
		if w = strings.TrimSpace(strings.ToLower(w)); w != "" {
			norm = append(norm, w)
		}
	}
	return &WordListModerator{words: norm, action: action}
}

// UpdateWords 热更新词表（管理端后续扩展用；并发安全）。
func (m *WordListModerator) UpdateWords(words []string) {
	norm := make([]string, 0, len(words))
	for _, w := range words {
		if w = strings.TrimSpace(strings.ToLower(w)); w != "" {
			norm = append(norm, w)
		}
	}
	m.mu.Lock()
	m.words = norm
	m.mu.Unlock()
}

func (m *WordListModerator) verdict() Verdict {
	if m.action == "pending" {
		return VerdictPending
	}
	return VerdictReject
}

// InspectText 文本敏感词检查。词表为空恒放行。
func (m *WordListModerator) InspectText(_ context.Context, text string) Verdict {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.words) == 0 {
		return VerdictAllow
	}
	lower := strings.ToLower(text)
	for _, w := range m.words {
		if strings.Contains(lower, w) {
			return m.verdict()
		}
	}
	return VerdictAllow
}

// InspectFile 文件审核钩子：内置实现恒放行（v1 不做文件内容扫描）。
// 外接扫描器以自定义 Moderator 注入。
func (m *WordListModerator) InspectFile(_ context.Context, _ UploadMeta) Verdict {
	return VerdictAllow
}

// NoopModerator 空审核器（moderation.enabled=false 或未注入时的等价行为）。
type NoopModerator struct{}

func (NoopModerator) InspectText(context.Context, string) Verdict     { return VerdictAllow }
func (NoopModerator) InspectFile(context.Context, UploadMeta) Verdict { return VerdictAllow }
