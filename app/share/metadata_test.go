package share

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/model"
)

// 回归：/share/metadata 语义——查询不扣次数、不要求密码、不外泄文本内容/密码哈希。
func TestGetShareMetadata(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	// 文本分享：type=text，不返回内容
	textResp, err := svc.ShareTextWithAuth(ctx, "机密内容ABC", 1, "day", false, "", nil, "127.0.0.1", false, "")
	require.NoError(t, err)
	meta, err := svc.GetShareMetadata(ctx, textResp.Code)
	require.NoError(t, err)
	require.Equal(t, "text", meta.Type)
	require.Empty(t, meta.Name, "text 分享不得经 metadata 泄露内容")
	require.False(t, meta.HasPassword)

	// 带密码的文件分享：type=file、has_password=true；
	// 最小化返回（2026-10-05 审计 P2）：输对密码前不泄露文件名/大小/统计
	pwdHash, err := utils.HashPassword("s3cret")
	require.NoError(t, err)
	fileResp, err := svc.CreateShare(ctx, &ShareFileReq{
		Channel:      "direct",
		FilePath:     "uploads/2026/10/03/uuid.txt",
		Size:         123,
		Text:         "a.txt",
		ExpiredAt:    utils.CalculateExpireTime(1, "day"),
		ExpiredCount: utils.CalculateExpireCount("day", 1),
		RequireAuth:  true,
		PasswordHash: pwdHash,
		UploadType:   "authenticated",
	})
	require.NoError(t, err)
	meta, err = svc.GetShareMetadata(ctx, fileResp.Code)
	require.NoError(t, err)
	require.Equal(t, "file", meta.Type)
	require.True(t, meta.HasPassword)
	require.Empty(t, meta.Name, "密码保护分享不得免密码泄露文件名")
	require.Zero(t, meta.Size, "密码保护分享不得免密码泄露大小")
	require.Zero(t, meta.FileCount, "密码保护分享不得免密码泄露子文件数")
	require.Equal(t, -1, meta.ExpiredCount)

	// 无密码文件分享：全量字段（文件名可见）
	openResp, err := svc.CreateShare(ctx, &ShareFileReq{
		Channel:      "direct",
		FilePath:     "uploads/2026/10/03/open.txt",
		Size:         5,
		Text:         "open.txt",
		ExpiredAt:    utils.CalculateExpireTime(1, "day"),
		ExpiredCount: utils.CalculateExpireCount("day", 1),
		UploadType:   "authenticated",
	})
	require.NoError(t, err)
	openMeta, err := svc.GetShareMetadata(ctx, openResp.Code)
	require.NoError(t, err)
	require.Equal(t, "open.txt", openMeta.Name)
	require.False(t, openMeta.HasPassword)
	require.Equal(t, int64(1), openMeta.FileCount)

	// 查询不扣次数
	require.Equal(t, meta.UsedCount, func() int {
		m, _ := svc.GetShareMetadata(ctx, fileResp.Code)
		return m.UsedCount
	}())

	// 已过期：报错（handler 统一转 404）
	past := time.Now().Add(-time.Hour)
	expired, err := svc.CreateShare(ctx, &ShareFileReq{
		Channel:      "direct",
		FilePath:     "uploads/2026/10/03/old.txt",
		Size:         1,
		Text:         "old.txt",
		ExpiredAt:    &past,
		ExpiredCount: -1,
		UploadType:   "authenticated",
	})
	require.NoError(t, err)
	_, err = svc.GetShareMetadata(ctx, expired.Code)
	require.Error(t, err, "过期分享应拒绝元数据查询")
}

// 回归（2026-10-24）：取件次数用完后，元数据面**不得**再回显文件名/大小。
//
// 缺陷：model.FileCode.IsExpired() 拆分前含次数语义（ExpiredCount==0 ⇒ 过期）
// ⇒ 本面在额度用完后报「已过期」；拆分后 IsExpired() 只判时间 ⇒ 本面漏掉了
// 次数闸 ⇒ 分享已失效但**文件名/大小/统计仍可被任意持码者读走**（旧口径 404）。
//
// 错误码口径：耗尽给 ErrShareCountExhausted（typed）⇒ handler 映射 20004
// 「超过取件次数」，不再与「不存在/已过期」一起塌成 404。
//
// 断言必须能变红：把 app/share/metadata.go 里的 IsCountExhausted 闸门删掉，
// 本用例即失败。
func TestGetShareMetadata_CountExhausted(t *testing.T) {
	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()

	// count 样式 = 按取件次数（1 次）
	resp, err := svc.CreateShare(ctx, &ShareFileReq{
		Channel:      "direct",
		FilePath:     "uploads/2026/10/24/limited.txt",
		Size:         7,
		Text:         "limited.txt",
		ExpiredAt:    utils.CalculateExpireTime(1, "day"),
		ExpiredCount: utils.CalculateExpireCount("count", 1),
		UploadType:   "authenticated",
	})
	require.NoError(t, err)

	// 额度未用完 ⇒ 元数据正常返回
	meta, err := svc.GetShareMetadata(ctx, resp.Code)
	require.NoError(t, err, "额度未用完时元数据应可查")
	require.Equal(t, "limited.txt", meta.Name)

	// 额度用完 ⇒ 必须拒，且错误可被 handler 识别为「次数耗尽」
	require.NoError(t, gormDB.Model(&model.FileCode{}).
		Where("code = ?", resp.Code).
		Update("expired_count", 0).Error)

	_, err = svc.GetShareMetadata(ctx, resp.Code)
	require.Error(t, err, "额度用完后元数据面必须拒绝（否则泄露文件名/大小）")
	require.ErrorIs(t, err, ErrShareCountExhausted,
		"耗尽错误必须是 typed error，handler 据此映射 20004 而非笼统 404")
}
