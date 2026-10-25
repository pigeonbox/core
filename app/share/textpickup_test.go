package share

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/pkg/utils"
)

// =====================================================================
// 文本分享 6 位取件码铸造（2026-10-08 "只保留 6 位码"统一呈现）
// =====================================================================

// stubMinter 记录铸造调用的窄接口桩。
type stubMinter struct {
	calls int
	shareCode,
	fileName string
	fileSize    int64
	requireAuth bool
	expireAt    *time.Time
}

func (m *stubMinter) MintForShare(_ context.Context, shareCode, fileName string, fileSize int64, requireAuth bool, expireAt *time.Time) (string, error) {
	m.calls++
	m.shareCode, m.fileName, m.fileSize, m.requireAuth, m.expireAt =
		shareCode, fileName, fileSize, requireAuth, expireAt
	return "ABC234", nil
}

func TestShareTextMintsPickupCode(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	minter := &stubMinter{}
	svc.SetPickupMinter(minter)

	expireAt := time.Now().Add(time.Hour)
	resp, err := svc.ShareText(context.Background(), &ShareTextReq{
		Text:         "hello pickup code",
		ExpiredAt:    &expireAt,
		ExpiredCount: -1,
		UploadType:   "anonymous",
	})
	require.NoError(t, err)
	assert.Equal(t, "ABC234", resp.PickupCode, "文本分享应与文件分享同权铸造取件码")
	assert.Equal(t, 1, minter.calls)
	assert.Equal(t, resp.Code, minter.shareCode)
	assert.Equal(t, int64(len("hello pickup code")), minter.fileSize)
	require.NotNil(t, minter.expireAt)
}

func TestShareTextE2ESkipsMint(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	minter := &stubMinter{}
	svc.SetPickupMinter(minter)

	expireAt := time.Now().Add(time.Hour)
	resp, err := svc.ShareText(context.Background(), &ShareTextReq{
		Text: "ciphertext", ExpiredAt: &expireAt, ExpiredCount: -1,
		Encrypted: true, // E2E 密文：密钥只随链接，凭码取件无意义
	})
	require.NoError(t, err)
	assert.Empty(t, resp.PickupCode)
	assert.Equal(t, 0, minter.calls, "E2E 文本不应铸造取件码")
}

func TestTextPreviewSanitize(t *testing.T) {
	// 竖线/换行会破坏 KV meta 的 '|' 分隔格式，必须折叠
	got := utils.TextPreview("第一行\n第二行|带竖线")
	assert.NotContains(t, got, "\n")
	assert.NotContains(t, got, "|")

	// 超长截断到 32 rune + 省略号
	long := strings.Repeat("码", 50)
	got = utils.TextPreview(long)
	runes := []rune(got)
	assert.Len(t, runes, 33)
	assert.True(t, strings.HasSuffix(got, "…"))
}
