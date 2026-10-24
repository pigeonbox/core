// 取件元数据查询（对标上游 /share/metadata）。
// 语义：查询不扣次数、不要求密码；供取件页在输入密码前渲染文件名/大小/有效期。
// 密码只暴露「是否设置」，文本内容/文件路径/密码哈希永不外泄。
package share

import (
	"context"
	"time"
)

// ShareMetadata 取件元数据（公开展示字段）
type ShareMetadata struct {
	Code string `json:"code"`
	Type string `json:"type"` // text | file
	// Name 文件名（仅 file 分享；text 分享不返回内容）
	Name         string     `json:"name,omitempty"`
	Size         int64      `json:"size"`
	HasPassword  bool       `json:"has_password"`
	ExpiredAt    *time.Time `json:"expired_at"`
	ExpiredCount int        `json:"expired_count"` // -1 无限 / 0 耗尽 / >0 剩余
	UsedCount    int        `json:"used_count"`
	Encrypted    bool       `json:"encrypted"`
	// FileCount 多文件分享的子文件数（单文件/text 分享为 0）
	FileCount int64     `json:"file_count"`
	CreatedAt time.Time `json:"created_at"`
}

// GetShareMetadata 查询分享元数据（复用 GetFileByCode 的过期/封禁检查与 typed error）。
// 不存在/已过期/被封禁统一由调用方转 404，避免存在性区分扩大探测面。
//
// 密码保护分享的最小化返回（2026-10-05 审计 P2）：持码者在输对密码前只拿到
// has_password/type/encrypted——文件名常含敏感信息（简历/合同名），此前免密码
// 即泄露，且该端点无鉴权。普通分享仍返回全量字段。
func (s *Service) GetShareMetadata(ctx context.Context, code string) (*ShareMetadata, error) {
	fc, err := s.GetFileByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	// 占位分享（文件未上传）：与 select/download/preview 面同口径 —— 报
	// ErrShareNotReady（handler 映射 20001），而不是报 type=file 让前端
	// 渲染出一个"存在但下不到"的文件卡（口径分叉）。
	if IsShareNotReady(fc) {
		return nil, ErrShareNotReady
	}
	// 次数已用完 ⇒ 不再返回元信息（2026-10-24）：IsExpired 拆出「次数耗尽」后，
	// 本面少了这道闸 ⇒ 次数用完的分享**仍会泄露文件名/大小**（旧口径是 404）。
	// 用 typed error（而非内联 errors.New）⇒ handler 能把它映射到规范码
	// 20004「超过取件次数」，而不是和「不存在/已过期」一起塌成 404。
	if fc.IsCountExhausted() {
		return nil, ErrShareCountExhausted
	}
	meta := &ShareMetadata{
		Code:         fc.Code,
		Size:         fc.Size,
		HasPassword:  fc.RequireAuth && fc.PasswordHash != "",
		ExpiredAt:    fc.ExpiredAt,
		ExpiredCount: fc.ExpiredCount,
		UsedCount:    fc.UsedCount,
		Encrypted:    fc.Encrypted,
		CreatedAt:    fc.CreatedAt,
	}
	if fc.FilePath == "" && fc.UUIDFileName == "" {
		meta.Type = "text"
	} else {
		meta.Type = "file"
		meta.Name = fc.Text // 约定：文件分享的 Text 字段存展示文件名（CreateShare 链路如此）
		if n, cerr := s.fileFileRepo.CountByFileCodeID(ctx, fc.ID); cerr == nil {
			meta.FileCount = n
		}
	}
	if meta.HasPassword {
		// 保留 code/type/has_password/encrypted 与过期时间（供前端渲染锁样式），
		// 抹去内容性字段：文件名/大小/取件统计/创建时间
		meta.Name = ""
		meta.Size = 0
		meta.UsedCount = 0
		meta.ExpiredCount = -1
		meta.FileCount = 0
		meta.CreatedAt = time.Time{}
	}
	return meta, nil
}
