// 内存内容直传分享（MCP/AI 通道，2026-10-06）：AI 客户端经 MCP 端点上传小文件
// （base64 随 JSON-RPC 携带），服务端写入统一存储后走与普通上传完全相同的
// CreateShare 链路（配额/审核/自定义码/联邦公告）。与 localimport 的差异仅在
// 内容来源：无白名单路径校验，改为文件名消毒 + 扩展名白名单 + 大小上限把关。
package share

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/pigeonbox/core/pkg/utils"
)

// ShareBytesOpts 内存内容建分享参数
type ShareBytesOpts struct {
	FileName     string
	Content      []byte
	ExpireValue  int
	ExpireStyle  string
	PasswordHash string
	CustomCode   string
	UserID       *uint
	OwnerIP      string
}

// ShareBytes 把内存中的文件内容登记为文件分享。
// 校验链：文件名消毒 → 扩展名白名单 → 大小上限 → SaveBytes 落存储 → CreateShare。
func (s *Service) ShareBytes(ctx context.Context, opts ShareBytesOpts) (*ShareResp, error) {
	name := utils.SanitizeFileName(opts.FileName)
	if name == "" {
		return nil, errors.New("文件名无效")
	}
	if !utils.IsAllowedExtension(name) {
		return nil, errors.New("该文件类型禁止上传")
	}
	if err := utils.CheckUploadSize(int64(len(opts.Content)), utils.GetMaxFileSize()); err != nil {
		return nil, utils.NewFileTooLargeError(utils.GetMaxFileSize())
	}

	_, rel := utils.NewUploadRelPath(name)
	// 走 SaveStream（local/S3/webdav 通用，与本地导入同路径），避免为内存内容扩接口
	if _, err := s.storage.SaveStream(ctx, rel, bytes.NewReader(opts.Content), int64(len(opts.Content))); err != nil {
		return nil, fmt.Errorf("写入存储失败: %w", err)
	}

	// 上传通道标签随归属推导（与 ShareTextWithAuth 同语义）：MCP 游客快速分享
	// （allow_guest）为匿名通道，登录用户/管理员为认证通道
	uploadType := "anonymous"
	if opts.UserID != nil {
		uploadType = "authenticated"
	}

	return s.CreateShare(ctx, &ShareFileReq{
		Channel:      "mcp",
		FilePath:     rel,
		Size:         int64(len(opts.Content)),
		Text:         name,
		ExpiredAt:    utils.CalculateExpireTime(opts.ExpireValue, opts.ExpireStyle),
		ExpiredCount: utils.CalculateExpireCount(opts.ExpireStyle, opts.ExpireValue),
		RequireAuth:  opts.PasswordHash != "",
		PasswordHash: opts.PasswordHash,
		UserID:       opts.UserID,
		UploadType:   uploadType,
		OwnerIP:      opts.OwnerIP,
		CustomCode:   opts.CustomCode,
	})
}
