package security

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 令牌 = **一次取件的凭证**（2026-10-24）：
//   - 形态 "exp.nonce.sig"（nonce 保证同秒签发的令牌互不相同 ⇒ 计次键可靠）；
//   - 计次指纹单向（HMAC），落库的是指纹不是令牌 ⇒ DB 泄漏不等于泄漏下载凭证。
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
	assert.False(t, VerifyDownloadToken(code, "123.abc.def"))
	// 过期时间戳
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3, "令牌形态应为 exp.nonce.sig")
	forged := parts[2]
	assert.False(t, VerifyDownloadToken(code, "123.abc."+forged))
}

func TestDownloadTokenUnsigned(t *testing.T) {
	// 未注入密钥：生成返回空串，校验恒 false（调用方按未启用处理）
	saved := downloadTokenSecret
	downloadTokenSecret = nil
	defer func() { downloadTokenSecret = saved }()

	assert.Empty(t, GenerateDownloadToken("AbCd1234"))
	assert.False(t, VerifyDownloadToken("AbCd1234", "1.aa"))
	assert.Empty(t, VerifyDownloadTokenFingerprint("AbCd1234", "1.aa"))
}

// TestDownloadTokenNonceUnique 同秒签发的令牌必须互不相同（2026-10-24）。
//
// 缺陷：此前载荷是 `code.exp`（秒级）⇒ 同一秒内签发的令牌**逐字节相同**。
// 计次键取令牌指纹 ⇒ 两个不同取件人被当成「同一次取件」⇒ 第二个不扣次数
// （额度漏扣，owner 定的次数上限被绕过）。
func TestDownloadTokenNonceUnique(t *testing.T) {
	SetDownloadTokenSecret("test-secret-32-chars-xxxxxxxxxxxxx")
	code := "AbCd1234"

	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		tok := GenerateDownloadToken(code)
		require.NotEmpty(t, tok)
		assert.False(t, seen[tok], "同秒内签发的令牌必须唯一（第 %d 个重复）", i)
		seen[tok] = true
	}
}

// TestDownloadTokenFingerprintStable 指纹：同一令牌稳定、不同令牌不同、非令牌明文
func TestDownloadTokenFingerprintStable(t *testing.T) {
	SetDownloadTokenSecret("test-secret-32-chars-xxxxxxxxxxxxx")
	code := "AbCd1234"

	tok := GenerateDownloadToken(code)
	fp := VerifyDownloadTokenFingerprint(code, tok)
	require.NotEmpty(t, fp, "有效令牌应给出计次指纹")
	assert.Equal(t, fp, VerifyDownloadTokenFingerprint(code, tok), "指纹稳定")
	assert.Equal(t, fp, DownloadTokenFingerprint(tok), "签发方算的指纹与校验方一致")

	tok2 := GenerateDownloadToken(code)
	assert.NotEqual(t, fp, DownloadTokenFingerprint(tok2), "不同令牌指纹不同")
	assert.NotContains(t, fp, tok, "指纹不得包含令牌明文")
	assert.NotEqual(t, tok, fp)

	// 无效令牌（换 code）⇒ 指纹为空（调用方据「空 = 无效」拒绝）
	assert.Empty(t, VerifyDownloadTokenFingerprint("other123", tok))
}

// TestDownloadTokenTTLFromConfig 令牌有效期接配置（此前配置项从未被读取）
func TestDownloadTokenTTLFromConfig(t *testing.T) {
	saved := downloadTokenTTLSec.Load()
	defer downloadTokenTTLSec.Store(saved)

	SetDownloadTokenTTL(60)
	assert.Equal(t, 60*time.Second, DownloadTokenTTL())
	SetDownloadTokenTTL(0) // 非法值 ⇒ 回落默认
	assert.Equal(t, defaultDownloadTokenTTL, DownloadTokenTTL())
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
