package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
)

// core#165 回归：validateSecrets 此前只判「空 + 7 项黑名单」，错误文案承诺的
// (>=32 chars) 从未被实现 ⇒ PB_JWT_SECRET=short 等任意非黑名单弱值可启动。
// HS256 配弱密钥：低权限用户拿到自己一枚 JWT 即可离线爆破出密钥 ⇒ 伪造任意 admin JWT。
//
// 断言必须能变红：抽掉 validateSecrets 的长度/熵判据 ⇒ 短值用例当场红。

// 通过门禁的合法强密钥（既有用例同款，48 字符、混合字符集）。
const strongJWTSecret = "test-only-jwt-secret-not-a-default-value-0123456789"

// TestInitConfig_JWTSecretShort_Rejected 短值必须启动被拒（负对照靶心）。
// 修复前：非黑名单短值静默通过 fail-fast ⇒ 本用例红。
func TestInitConfig_JWTSecretShort_Rejected(t *testing.T) {
	for _, sec := range []string{
		"short",                           // issue 复现原值
		"a",                               // 1 字符
		"123456",                          // 6 字符全数字
		"secret",                          // 常见占位
		"0123456789012345678901234567890", // 31 字符：差 1 位到门槛
	} {
		t.Run(sec, func(t *testing.T) {
			p := writeTestConfigYAML(t, "user:\n  jwt_secret: "+sec+"\n")
			_, err := InitConfig(p)
			require.Error(t, err, "弱 jwt_secret %q 不得通过启动门禁", sec)
			assert.Contains(t, err.Error(), "32",
				"报错必须点名长度门槛（与文案承诺的 >=32 chars 一致）：%s", err)
		})
	}
}

// TestInitConfig_JWTSecretStrong_Allowed ≥32 的强随机值必须放行（不能只拦不放）。
func TestInitConfig_JWTSecretStrong_Allowed(t *testing.T) {
	p := writeTestConfigYAML(t, "user:\n  jwt_secret: "+strongJWTSecret+"\n")
	cfg, err := InitConfig(p)
	require.NoError(t, err, "≥32 强随机密钥必须放行：%v", err)
	assert.Equal(t, strongJWTSecret, cfg.User.JWTSecret)
}

// TestInitConfig_JWTSecretBlacklist_StillRejected 黑名单仍拒，且报错口径是
// 「known default」而不是「too short」——排障不能被误导（core#165 修复方向之一：
// 把黑名单与长度检查的报错分开）。黑名单值本身多为短串，必须先判黑名单。
func TestInitConfig_JWTSecretBlacklist_StillRejected(t *testing.T) {
	for _, sec := range []string{
		"PigeonBox2025JWT",                  // user.jwt_secret 默认值
		"REPLACE_WITH_STRONG_RANDOM_SECRET", // k8s 占位值（长度 33，靠黑名单拦）
	} {
		t.Run(sec, func(t *testing.T) {
			p := writeTestConfigYAML(t, "user:\n  jwt_secret: "+sec+"\n")
			_, err := InitConfig(p)
			require.Error(t, err, "黑名单密钥 %q 必须拒绝", sec)
			assert.Contains(t, err.Error(), "known default",
				"黑名单命中须报 known default（与长度检查分开）：%s", err)
		})
	}
}

// TestValidateSecrets_WeakEntropy 长度够但零熵的值必须拒（issue 修复方向：
// 「简单熵判据（拒绝全数字/重复字符）」）。只判长度挡不住 32 位全 0。
func TestValidateSecrets_WeakEntropy(t *testing.T) {
	digits := "012345678901234567890123456789012" // 33 字符全数字
	repeat := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 33 字符单字符重复
	for _, sec := range []string{digits, repeat} {
		t.Run(sec[:8]+"...", func(t *testing.T) {
			err := validateSecrets(&Config{User: conf.UserConfig{JWTSecret: sec}})
			require.Error(t, err, "零熵密钥 %q 不得放行", sec)
			assert.Contains(t, err.Error(), "32", "报错须点名门槛：%s", err)
		})
	}
}
