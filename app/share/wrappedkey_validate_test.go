package share

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgerrors "github.com/pigeonbox/core/pkg/errors"
)

// =====================================================================
// 口令型 E2E 的 wrapped_key 形状校验（core#30 补缺）
//
// 校验只做「编码与长度」，不做「内容」⇒ 零知识保持（服务端没有口令，
// 任何校验都不可能解密）；也不钉死信封版本 ⇒ 不阻碍格式演进。
// =====================================================================

// validEnvelope 造一个**形状合法**的信封：标准 base64 编码的 77 字节
// （0x01||salt16||nonce12||ct32||tag16）。服务端不解析内容，
// 这里只保证编码与长度合法。
func validEnvelope() string {
	raw := make([]byte, 77)
	raw[0] = 0x01
	return base64.StdEncoding.EncodeToString(raw)
}

func TestValidateWrappedKey(t *testing.T) {
	t.Run("空串合法（明文 / 旧 URL-hash-key 形态）", func(t *testing.T) {
		require.NoError(t, ValidateWrappedKey(""))
	})
	t.Run("合法信封被接受", func(t *testing.T) {
		require.NoError(t, ValidateWrappedKey(validEnvelope()))
	})
	t.Run("非 base64 被拒", func(t *testing.T) {
		err := ValidateWrappedKey("这不是 base64!!!")
		require.Error(t, err)
		assert.Equal(t, pkgerrors.CodeBadRequest, errCode(err), "必须是 400/10001 参数错")
	})
	t.Run("base64url 变体被拒（前端用的是标准 base64，不是 base64url）", func(t *testing.T) {
		// base64url 用 '-' 与 '_' 替掉 '+' 与 '/' ⇒ 标准解码失败。
		err := ValidateWrappedKey("AQ-_AAAAAAAA")
		require.Error(t, err)
	})
	t.Run("解码后超长被拒（防存储/传输放大）", func(t *testing.T) {
		big := base64.StdEncoding.EncodeToString(make([]byte, wrappedKeyMaxBytes+1))
		err := ValidateWrappedKey(big)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "长度非法")
	})
	t.Run("空白串按未提供处理", func(t *testing.T) {
		require.NoError(t, validateWrappedKeyField("   "))
		require.NoError(t, validateWrappedKeyField(""))
	})
}

// errCode 从业务错误里取错误码（非业务错误返回 0）。
func errCode(err error) int {
	if be, ok := err.(*pkgerrors.AppError); ok {
		return be.BizCode
	}
	return 0
}

// TestWrappedKeyRejectedOnAllChannels 三个创建通道都必须拒非法 wrapped_key。
//
// 为什么是三条：校验若只加在一个入口 ⇒ 另外两个入口仍是「无校验落库」，
// 而 HTTP handler 有三条独立的调用路径（text / file / multi）⇒
// 断言必须打在**每个**通道上，否则漏一个就是静默放行。
func TestWrappedKeyRejectedOnAllChannels(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	bad := "这不是 base64!!!"

	// 1. 文本通道
	_, err := svc.ShareText(ctx, &ShareTextReq{
		Text: "ciphertext", ExpiredCount: -1,
		Encrypted: true, WrappedKey: bad,
	})
	require.Error(t, err, "文本通道必须拒非法 wrapped_key")
	assert.Equal(t, pkgerrors.CodeBadRequest, errCode(err))

	// 2. 文件通道
	_, err = svc.CreateShare(ctx, &ShareFileReq{
		Channel: "direct", FilePath: "uploads/2026/11/02/x.bin", Size: 8, Text: "x.bin",
		ExpiredCount: -1, Encrypted: true, WrappedKey: bad,
	})
	require.Error(t, err, "文件通道必须拒非法 wrapped_key")
	assert.Equal(t, pkgerrors.CodeBadRequest, errCode(err))

	// 3. 多文件通道
	_, err = svc.CreateMultiFileShare(ctx, &MultiShareReq{
		Entries:      []StoredFileEntry{{RelPath: "uploads/2026/11/02/a.bin", Size: 4}},
		ExpiredCount: -1, Encrypted: true, WrappedKey: bad,
	})
	require.Error(t, err, "多文件通道必须拒非法 wrapped_key")
	assert.Equal(t, pkgerrors.CodeBadRequest, errCode(err))
}

// TestWrappedKeyValidAcceptedOnAllChannels 合法信封在三通道都应被接受
// （负对照：上一条的「拒」不是「一律拒一切」造成的假红）。
func TestWrappedKeyValidAcceptedOnAllChannels(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	wk := validEnvelope()

	resp, err := svc.ShareText(ctx, &ShareTextReq{
		Text: "ciphertext", ExpiredCount: -1,
		Encrypted: true, WrappedKey: wk,
	})
	require.NoError(t, err, "合法信封必须被接受")
	assert.Equal(t, wk, resp.WrappedKey, "服务端不得改写信封（原样存原样发）")

	_, err = svc.CreateShare(ctx, &ShareFileReq{
		Channel: "direct", FilePath: "uploads/2026/11/02/ok.bin", Size: 8, Text: "ok.bin",
		ExpiredCount: -1, Encrypted: true, WrappedKey: wk,
	})
	require.NoError(t, err)

	_, err = svc.CreateMultiFileShare(ctx, &MultiShareReq{
		Entries:      []StoredFileEntry{{RelPath: "uploads/2026/11/02/b.bin", Size: 4}},
		ExpiredCount: -1, Encrypted: true, WrappedKey: wk,
	})
	require.NoError(t, err)
}

// TestWrappedKeyNotTruncated 合法信封必须逐字回读（防 DB 列截断）。
//
// 落库列是 `size:256`；信封 base64 现状 104 字符 ⇒ 留足余量，但断言要钉住
// 「逐字一致」——一旦有人把列改窄或加了清洗，这里会红。
func TestWrappedKeyNotTruncated(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	wk := validEnvelope()

	resp, err := svc.ShareText(ctx, &ShareTextReq{
		Text: "ciphertext", ExpiredCount: -1,
		Encrypted: true, WrappedKey: wk,
	})
	require.NoError(t, err)

	fc, err := svc.GetFileByCode(ctx, resp.Code)
	require.NoError(t, err)
	assert.Equal(t, wk, fc.WrappedKey, "落库值必须逐字一致（不得截断/改写）")
	assert.Equal(t, len(wk), len(strings.TrimSpace(fc.WrappedKey)))
}
