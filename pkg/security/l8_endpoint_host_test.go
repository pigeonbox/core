package security

// L8 · ssrf：裸主机端点（FTP/SFTP 的 host 字段）与「对端粒度放行私网」两条
// 此前 0 覆盖的 SSRF 判定。
//
// 威胁：ValidateEndpointHost 是 FTP/SFTP 配置的**唯一**出站地址防线——
// host 字段没有 scheme，不能走 ValidateEndpointURL（URL 解析那一档）。
// 该函数此前 0 覆盖 ⇒ 私网判定被改坏（如只判 127.0.0.1、不判 RFC1918）
// 不会有任何测试变红，FTP/SFTP 后端可直接指向内网（SSRF）。
//
// ValidateEndpointURLAllowPrivate 是联邦投递的「按对端粒度放行私网」入口：
// 它**只**放松私网判定，不得顺手放松 scheme/host 校验
// （javascript:/data: 这类可执行 scheme 必须照拒）。

import (
	"testing"

	"github.com/pigeonbox/core/conf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// l8WithConfig 替换全局配置（SSRF 判定读全局开关）并在用例结束后复位。
func l8WithConfig(t *testing.T, cfg *conf.AppConfiguration) {
	t.Helper()
	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(cfg)
	t.Cleanup(func() { conf.SetGlobalConfig(old) })
}

// TestL8_ValidateEndpointHostRejectsPrivateTargets 裸主机端点必须拒私网/回环/元数据地址。
//
// 全部用 IP 字面量 ⇒ 不依赖 DNS（解析失败会被判据误当成「放行」）。
func TestL8_ValidateEndpointHostRejectsPrivateTargets(t *testing.T) {
	l8WithConfig(t, &conf.AppConfiguration{})

	for _, host := range []string{
		"127.0.0.1",          // 回环
		"127.0.0.1:21",       // 回环 + 端口
		"10.0.0.5",           // RFC1918
		"10.0.0.5:9000",      // RFC1918 + 端口
		"192.168.1.10",       // RFC1918
		"172.16.0.1",         // RFC1918
		"169.254.169.254",    // 云元数据地址（SSRF 头号目标）
		"169.254.169.254:80", // 云元数据 + 端口
		"0.0.0.0",            // 未指定
		"::1",                // IPv6 回环
		"[::1]:21",           // IPv6 回环 + 端口
		"fe80::1",            // IPv6 链路本地
	} {
		err := ValidateEndpointHost(host)
		require.Error(t, err, "裸主机端点必须拒绝私网/回环地址：%s", host)
		assert.ErrorIs(t, err, ErrEndpointURL)
	}
}

// TestL8_ValidateEndpointHostAllowsPublicTarget 公网地址必须放行（判据没把路全堵死）。
func TestL8_ValidateEndpointHostAllowsPublicTarget(t *testing.T) {
	l8WithConfig(t, &conf.AppConfiguration{})

	require.NoError(t, ValidateEndpointHost("8.8.8.8"))
	require.NoError(t, ValidateEndpointHost("8.8.8.8:21"))
}

// TestL8_ValidateEndpointHostRejectsEmpty 空主机必须拒（否则会退化成"无限制"）。
func TestL8_ValidateEndpointHostRejectsEmpty(t *testing.T) {
	l8WithConfig(t, &conf.AppConfiguration{})

	err := ValidateEndpointHost("")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEndpointURL)

	err = ValidateEndpointHost("   ")
	require.Error(t, err, "空白主机必须按缺 host 拒")
	assert.ErrorIs(t, err, ErrEndpointURL)
}

// TestL8_ValidateEndpointHostRespectsAllowPrivateFlag 全局开关开启 ⇒ 私网放行。
func TestL8_ValidateEndpointHostRespectsAllowPrivateFlag(t *testing.T) {
	cfg := &conf.AppConfiguration{}
	cfg.Security.SSRF.AllowPrivateNetworks = true
	l8WithConfig(t, cfg)

	require.NoError(t, ValidateEndpointHost("127.0.0.1:21"))
	require.NoError(t, ValidateEndpointHost("192.168.1.10"))
}

// TestL8_ValidateEndpointURLAllowPrivateKeepsSchemeGuard 对端粒度放行**只**放松私网判定。
//
// 负对照：把 validateEndpointURL 的 allowPrivate 分支提前 return（连 scheme/host
// 一起放行）⇒ 本用例转红（javascript:/data: 端点被接受）。
func TestL8_ValidateEndpointURLAllowPrivateKeepsSchemeGuard(t *testing.T) {
	l8WithConfig(t, &conf.AppConfiguration{})

	// 私网地址放行（对端粒度）
	require.NoError(t, ValidateEndpointURLAllowPrivate("http://192.168.1.10:9000"))
	require.NoError(t, ValidateEndpointURLAllowPrivate("http://127.0.0.1:9000"))

	// scheme 白名单不放宽
	err := ValidateEndpointURLAllowPrivate("ftp://minio.local:9000")
	require.Error(t, err, "放行私网不等于放行 ftp")
	assert.ErrorIs(t, err, ErrEndpointURL)

	err = ValidateEndpointURLAllowPrivate("file:///etc/passwd")
	require.Error(t, err, "放行私网不等于放行 file")
	assert.ErrorIs(t, err, ErrEndpointURL)

	err = ValidateEndpointURLAllowPrivate("javascript:alert(1)")
	require.Error(t, err, "放行私网不等于放行 javascript")
	assert.ErrorIs(t, err, ErrEndpointURL)

	// host 必填不放宽
	err = ValidateEndpointURLAllowPrivate("http://")
	require.Error(t, err, "放行私网不等于放行空 host")
	assert.ErrorIs(t, err, ErrEndpointURL)
}
