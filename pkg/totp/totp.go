// Package totp 实现 RFC 6238 的 TOTP（time-based one-time password）。
//
// 只做最小实现：SHA1 / 30 秒步长 / 6 位数字 —— 这是 Google Authenticator、
// 1Password、Bitwarden、Authy 等客户端默认能吃下的唯一公共子集。
// 选 SHA1 不是「图省事」：RFC 6238 允许 SHA256/SHA512，但相当一部分移动端
// 认证器只实现 SHA1 分支；管理员二验是**唯一的找回路径是恢复码**的场景，
// 兼容性优先于强度（6 位数字 + 30s 窗口的熵已足够对抗在线爆破，离线爆破
// 需要拿到密钥本身，那时哈希算法已不是瓶颈）。
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// SecretBytes 密钥长度（字节）。RFC 4226 §4 建议 160 位 = SHA1 输出长度。
	SecretBytes = 20
	// Period 时间步长。
	Period = 30 * time.Second
	// Digits 输出位数。
	Digits = 6
	// Skew 容忍的时间窗个数（前后各 Skew 个）。
	//
	// 取 1 而不是 0：手机与服务器的时钟常有几秒漂移，0 会让「刚刚好卡在
	// 边界」的用户被判错码；取 2 以上则把有效码数量放大到 5 个以上，
	// 在线爆破的搜索空间明显变小。1 是这两者之间的常用折中。
	Skew = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret 生成一个新的 base32（无填充）密钥。
func GenerateSecret() (string, error) {
	buf := make([]byte, SecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return b32.EncodeToString(buf), nil
}

// normalizeSecret 去掉空格、连字符与 padding，统一大写。
//
// 用户手工输入密钥时几乎一定会带空格或小写；不归一化会得到一个
// 「密钥非空但永远算不对」的死局，而这种故障只能靠日志看出来。
func normalizeSecret(secret string) string {
	s := strings.ToUpper(strings.TrimSpace(secret))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.TrimRight(s, "=")
	return s
}

// Code 计算 t 时刻的验证码。secret 为 base32 编码的密钥。
func Code(secret string, t time.Time) (string, error) {
	key, err := b32.DecodeString(normalizeSecret(secret))
	if err != nil {
		return "", fmt.Errorf("totp: 密钥不是合法 base32: %w", err)
	}
	if len(key) == 0 {
		return "", fmt.Errorf("totp: 密钥为空")
	}
	counter := uint64(t.Unix() / int64(Period/time.Second))

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	// RFC 4226 §5.3 动态截断
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff

	mod := uint32(1)
	for i := 0; i < Digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", Digits, bin%mod), nil
}

// Validate 校验验证码是否落在容忍窗口内。
//
// code 会先去掉空白与常见分隔符（用户从认证器复制时常带空格）。
func Validate(secret, code string, t time.Time) bool {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != Digits {
		return false
	}
	for d := -Skew; d <= Skew; d++ {
		want, err := Code(secret, t.Add(time.Duration(d)*Period))
		if err != nil {
			return false
		}
		// 定长比较：验证码长度固定，用常数时间比较避免按字节提前返回。
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

// ProvisioningURL 生成 otpauth:// URL（认证器扫码用）。
func ProvisioningURL(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", normalizeSecret(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprintf("%d", Digits))
	q.Set("period", fmt.Sprintf("%d", int(Period/time.Second)))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// RecoveryCodeCount 一次性恢复码个数。
const RecoveryCodeCount = 8

// GenerateRecoveryCodes 生成 n 个恢复码（形如 ABCD-EFGH）。
//
// 恢复码是「手机丢了怎么进后台」的唯一兜底，所以故意用 base32 字母表
// （去掉易混的 0/O/1/I）并分组展示：用户抄在纸上时不会抄错。
func GenerateRecoveryCodes(n int) ([]string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		buf := make([]byte, 8)
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		sb := make([]byte, 0, 9)
		for j, b := range buf {
			if j == 4 {
				sb = append(sb, '-')
			}
			sb = append(sb, alphabet[int(b)%len(alphabet)])
		}
		out = append(out, string(sb))
	}
	return out, nil
}

// NormalizeRecoveryCode 归一化用户输入的恢复码（大小写与分隔符不敏感）。
func NormalizeRecoveryCode(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	return s
}
