package security

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDownloadTokenRoundTrip(t *testing.T) {
	SetDownloadTokenSecret("test-secret-32-chars-xxxxxxxxxxxxx")
	code := "AbCd1234"

	token := GenerateDownloadToken(code)
	require.NotEmpty(t, token)
	assert.True(t, VerifyDownloadToken(code, token), "正确 code+token 应通过")

	// 换 code 校验失败（令牌绑定分享码）
	assert.False(t, VerifyDownloadToken("other123", token))
	// 篡改签名失败
	assert.False(t, VerifyDownloadToken(code, token[:len(token)-2]+"xx"))
	// 格式错误
	assert.False(t, VerifyDownloadToken(code, "not-a-token"))
	assert.False(t, VerifyDownloadToken(code, "abc.def"))
	// 过期时间戳
	exp := time.Now().Add(-time.Hour).Unix()
	forged := strings.Split(token, ".")[1]
	assert.False(t, VerifyDownloadToken(code, "123."+forged))
	_ = exp
}

func TestDownloadTokenUnsigned(t *testing.T) {
	// 未注入密钥：生成返回空串，校验恒 false（调用方按未启用处理）
	saved := downloadTokenSecret
	downloadTokenSecret = nil
	defer func() { downloadTokenSecret = saved }()

	assert.Empty(t, GenerateDownloadToken("AbCd1234"))
	assert.False(t, VerifyDownloadToken("AbCd1234", "1.aa"))
}

// 令牌**扣次口径**（2026-10-24）：普通令牌 vs 「已扣次」令牌。
// 背景：匿名取件 retrieve 扣过一次取件次数，随它下发的直链下载时不得再扣一次
// （此前一次取件扣 2 次，count 样式下对方可用次数减半）。
func TestDownloadTokenFlavor(t *testing.T) {
	SetDownloadTokenSecret("test-secret-32-chars-xxxxxxxxxxxxx")
	code := "AbCd1234"

	normal := GenerateDownloadToken(code)
	paid := GeneratePaidDownloadToken(code)
	require.NotEmpty(t, normal)
	require.NotEmpty(t, paid)

	// 口径判定
	assert.Equal(t, TokenNormal, VerifyDownloadTokenFlavor(code, normal), "普通令牌 ⇒ 普通口径")
	assert.Equal(t, TokenPaid, VerifyDownloadTokenFlavor(code, paid), "已扣次令牌 ⇒ 已扣次口径")

	// 两种口径都算**有效**（有效性 ≠ 扣次口径）
	assert.True(t, VerifyDownloadToken(code, normal))
	assert.True(t, VerifyDownloadToken(code, paid))

	// 口径不可互相冒充：已扣次令牌按普通口径验不过（签名载荷不同）
	assert.False(t, VerifyDownloadTokenFlavor(code, normal) == TokenPaid)
	assert.False(t, VerifyDownloadTokenFlavor(code, paid) == TokenNormal)
	// 换 code 一律无效（令牌绑定分享码）
	assert.Equal(t, TokenInvalid, VerifyDownloadTokenFlavor("other123", paid))
	assert.Equal(t, TokenInvalid, VerifyDownloadTokenFlavor("other123", normal))
	// 篡改签名无效
	bad := paid[:len(paid)-2] + "xx"
	assert.Equal(t, TokenInvalid, VerifyDownloadTokenFlavor(code, bad))
	// 格式错误无效
	assert.Equal(t, TokenInvalid, VerifyDownloadTokenFlavor(code, "not-a-token"))
}

func TestGeneratePaidDownloadTokenUnsigned(t *testing.T) {
	saved := downloadTokenSecret
	downloadTokenSecret = nil
	defer func() { downloadTokenSecret = saved }()

	assert.Empty(t, GeneratePaidDownloadToken("AbCd1234"))
	assert.Equal(t, TokenInvalid, VerifyDownloadTokenFlavor("AbCd1234", "1.aa"))
}

func TestValidateEndpointURL(t *testing.T) {
	// scheme 白名单
	assert.ErrorIs(t, ValidateEndpointURL("ftp://minio.local:9000"), ErrEndpointURL)
	assert.ErrorIs(t, ValidateEndpointURL("file:///etc/passwd"), ErrEndpointURL)
	assert.ErrorIs(t, ValidateEndpointURL("http://"), ErrEndpointURL)
	// 公网 URL 通过（含 DNS 解析复判；example.com 解析结果为公网）
	assert.NoError(t, ValidateEndpointURL("https://example.com"))
	// 私网直写（默认拒绝；allow_private 未开）
	assert.ErrorIs(t, ValidateEndpointURL("http://192.168.1.10:9000"), ErrEndpointURL)
	assert.ErrorIs(t, ValidateEndpointURL("http://127.0.0.1:9000"), ErrEndpointURL)
	assert.ErrorIs(t, ValidateEndpointURL("http://10.0.0.5:5006"), ErrEndpointURL)
}
