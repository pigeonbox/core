package security

// L8 · pickup_enum：取件计次键（TextViewKey / DownloadSessionKey）此前 0 覆盖。
//
// 威胁：这两把 HMAC 键是「一次取件 = 一次交付」的判重凭据：
//   - TextViewKey(code, ip)：文本分享的计次键（同一 IP 刷新只扣 1 次）；
//   - DownloadSessionKey(code, ip)：关闭下载令牌时的兜底取件会话键。
//
// 两把键错了会直接变成计次语义漏洞：
//   - 键不稳定/不区分 IP ⇒ 额度漏扣或被恶意刷穿；
//   - 键跨域相同 ⇒ 文本面与文件面的计次互相串味（一面的取件吃掉另一面的额度）；
//   - 键里含明文 IP ⇒ 落库即明文 PII 泄漏（HMAC 的单向性被破坏）。
//
// 判据：非空且稳定、按 IP/码区分、两把键互不相同（域分离）、不含明文 IP。

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// l8SetTokenSecret 注入下载令牌密钥（两把计次键都读它）。
func l8SetTokenSecret(t *testing.T) {
	t.Helper()
	SetDownloadTokenSecret("l8-download-token-secret-32chars!!")
	t.Cleanup(func() { downloadTokenSecretPtr.Store(nil) })
}

// TestL8_TextViewKeyIsStableAndPerIPCoded 文本计次键：稳定、按 IP/码区分。
func TestL8_TextViewKeyIsStableAndPerIPCoded(t *testing.T) {
	l8SetTokenSecret(t)

	k1 := TextViewKey("AbCd1234", "1.2.3.4")
	k2 := TextViewKey("AbCd1234", "1.2.3.4")
	require.NotEmpty(t, k1)
	assert.Equal(t, k1, k2, "同一 (码, IP) 的计次键必须稳定，否则每次取件都算新的")

	assert.NotEqual(t, k1, TextViewKey("AbCd1234", "5.6.7.8"), "不同 IP 必须是不同的取件")
	assert.NotEqual(t, k1, TextViewKey("Zz9999", "1.2.3.4"), "不同分享码必须是不同的取件")
}

// TestL8_DownloadSessionKeyIsStableAndPerIPCoded 兜底取件会话键：同上口径。
func TestL8_DownloadSessionKeyIsStableAndPerIPCoded(t *testing.T) {
	l8SetTokenSecret(t)

	k1 := DownloadSessionKey("AbCd1234", "1.2.3.4")
	k2 := DownloadSessionKey("AbCd1234", "1.2.3.4")
	require.NotEmpty(t, k1)
	assert.Equal(t, k1, k2)

	assert.NotEqual(t, k1, DownloadSessionKey("AbCd1234", "5.6.7.8"))
	assert.NotEqual(t, k1, DownloadSessionKey("Zz9999", "1.2.3.4"))
}

// TestL8_DownloadSessionKeyDomainSeparated 两把键必须域分离。
//
// 域相同 ⇒ 文本面与文件面共用一把键 ⇒ 一次取件同时吃掉两个面的额度
// （或反之：额度被绕过）。域前缀分别是 "text|" 与 "ip|"。
func TestL8_DownloadSessionKeyDomainSeparated(t *testing.T) {
	l8SetTokenSecret(t)

	assert.NotEqual(t,
		TextViewKey("AbCd1234", "1.2.3.4"),
		DownloadSessionKey("AbCd1234", "1.2.3.4"),
		"文本面与文件面的计次键必须域分离，否则两个面的取件额度互相串味")
}

// TestL8_ChargeKeysDoNotLeakPlaintextIP 计次键不得含明文 IP（落库即 PII 泄漏）。
func TestL8_ChargeKeysDoNotLeakPlaintextIP(t *testing.T) {
	l8SetTokenSecret(t)

	ip := "203.0.113.7"
	code := "AbCd1234"

	for name, key := range map[string]string{
		"TextViewKey":        TextViewKey(code, ip),
		"DownloadSessionKey": DownloadSessionKey(code, ip),
	} {
		require.NotEmpty(t, key)
		assert.NotContains(t, key, ip, "%s 不得包含明文 IP", name)
		assert.NotContains(t, key, code, "%s 不得包含明文分享码", name)
	}
}

// TestL8_ChargeKeysEmptyWithoutSecretOrIP 未注入密钥或取不到 IP ⇒ 键为空（调用方按未启用处理）。
func TestL8_ChargeKeysEmptyWithoutSecretOrIP(t *testing.T) {
	l8SetTokenSecret(t)

	assert.Empty(t, TextViewKey("AbCd1234", ""), "取不到 IP 时不得产出计次键（否则全网共用一把）")
	assert.Empty(t, DownloadSessionKey("AbCd1234", ""))

	downloadTokenSecretPtr.Store(nil)
	assert.Empty(t, TextViewKey("AbCd1234", "1.2.3.4"), "未注入密钥时不得产出计次键")
	assert.Empty(t, DownloadSessionKey("AbCd1234", "1.2.3.4"))
}
