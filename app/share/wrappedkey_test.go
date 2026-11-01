package share

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/app/moderation"
)

// =====================================================================
// 口令型 E2E（core#30）：wrapped_key 信封密钥的铸造语义与落库回读。
//
// 语义基线（与旧 URL-hash-key 形态并存，互不放宽）：
//   - Encrypted + WrappedKey 非空 = 口令型 E2E ⇒ **铸 6 位取件码**（两因子：码+口令）
//   - Encrypted + WrappedKey 空   = 旧 URL-hash-key 形态 ⇒ 不铸码（密钥在 URL fragment）
//   - wrapped_key 服务端零知识：只存只发，不解析不解密
// =====================================================================

const testWrappedKey = "AQ==" + "AAAAAAAAAAAAAAAAAAAAAA" // 形状不校验，服务端只存只发

func TestShareTextPasswordWrappedMintsPickupCode(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	minter := &stubMinter{}
	svc.SetPickupMinter(minter)

	expireAt := time.Now().Add(time.Hour)
	resp, err := svc.ShareText(context.Background(), &ShareTextReq{
		Text: "ciphertext", ExpiredAt: &expireAt, ExpiredCount: -1,
		Encrypted:  true,
		WrappedKey: testWrappedKey,
	})
	require.NoError(t, err)
	assert.Equal(t, "ABC234", resp.PickupCode,
		"口令型 E2E 应铸造取件码（取件码+口令两因子）")
	assert.Equal(t, 1, minter.calls)
}

func TestShareTextE2EWithoutWrappedKeyStillSkipsMint(t *testing.T) {
	// 旧 URL-hash-key 形态（无 wrapped_key）保持不铸码——负对照，
	// 防「口令型铸码」改动顺手放宽旧形态语义。
	svc, _, _, _ := newTestService(t)
	minter := &stubMinter{}
	svc.SetPickupMinter(minter)

	expireAt := time.Now().Add(time.Hour)
	resp, err := svc.ShareText(context.Background(), &ShareTextReq{
		Text: "ciphertext", ExpiredAt: &expireAt, ExpiredCount: -1,
		Encrypted: true,
	})
	require.NoError(t, err)
	assert.Empty(t, resp.PickupCode)
	assert.Equal(t, 0, minter.calls)
}

func TestWrappedKeyPersistedAndEchoed(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	resp, err := svc.ShareText(ctx, &ShareTextReq{
		Text: "ciphertext", ExpiredCount: -1,
		Encrypted:  true,
		WrappedKey: testWrappedKey,
	})
	require.NoError(t, err)
	assert.Equal(t, testWrappedKey, resp.WrappedKey, "创建响应应回显信封密钥（属主可见）")

	fc, err := svc.GetFileByCode(ctx, resp.Code)
	require.NoError(t, err)
	assert.Equal(t, testWrappedKey, fc.WrappedKey, "wrapped_key 必须落库（真相源 = file_codes 列）")
	assert.True(t, fc.IsPasswordWrapped())
	assert.True(t, fc.Encrypted, "口令型 E2E 仍是加密分享")
}

func TestWrappedKeyFileShare(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	resp, err := svc.CreateShare(ctx, &ShareFileReq{
		Channel:      "direct",
		FilePath:     "uploads/2026/11/01/uuid.bin",
		Size:         64,
		Text:         "a.bin",
		ExpiredCount: -1,
		Encrypted:    true,
		WrappedKey:   testWrappedKey,
		UploadType:   "authenticated",
	})
	require.NoError(t, err)
	assert.Equal(t, testWrappedKey, resp.WrappedKey)

	fc, err := svc.GetFileByCode(ctx, resp.Code)
	require.NoError(t, err)
	assert.True(t, fc.IsPasswordWrapped())
}

func TestShareMetadataHasWrappedKey(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	// 口令型 E2E ⇒ has_wrapped_key=true（只给标志不给本体）
	wrapped, err := svc.ShareText(ctx, &ShareTextReq{
		Text: "ciphertext", ExpiredCount: -1,
		Encrypted: true, WrappedKey: testWrappedKey,
	})
	require.NoError(t, err)
	meta, err := svc.GetShareMetadata(ctx, wrapped.Code)
	require.NoError(t, err)
	assert.True(t, meta.HasWrappedKey)
	assert.True(t, meta.Encrypted)

	// 旧 URL-hash-key 形态 ⇒ false（不是缺席——字段恒在，值区分形态）
	legacy, err := svc.ShareText(ctx, &ShareTextReq{
		Text: "ciphertext", ExpiredCount: -1,
		Encrypted: true,
	})
	require.NoError(t, err)
	meta, err = svc.GetShareMetadata(ctx, legacy.Code)
	require.NoError(t, err)
	assert.False(t, meta.HasWrappedKey)

	// 明文分享 ⇒ false
	plain, err := svc.ShareText(ctx, &ShareTextReq{Text: "hi", ExpiredCount: -1})
	require.NoError(t, err)
	meta, err = svc.GetShareMetadata(ctx, plain.Code)
	require.NoError(t, err)
	assert.False(t, meta.HasWrappedKey)
}

func TestWrappedKeyNotModerated(t *testing.T) {
	// 口令型 E2E 的密文跳过审核（服务端零知识无明文可审）——
	// 与旧 E2E 同款，防止 wrapped_key 改动顺手把密文送进敏感词审核。
	svc, _, _, _ := newTestService(t)
	svc.SetModerator(&mockModerator{verdict: moderation.VerdictReject})

	expireAt := time.Now().Add(time.Hour)
	_, err := svc.ShareText(context.Background(), &ShareTextReq{
		Text: "ciphertext", ExpiredAt: &expireAt, ExpiredCount: -1,
		Encrypted: true, WrappedKey: testWrappedKey,
	})
	require.NoError(t, err, "密文分享不应被审核拦截")
}
