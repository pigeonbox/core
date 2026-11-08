package conf

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	mapstructure "github.com/go-viper/mapstructure/v2"
	"github.com/oekit/pkg/secretx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/internal/sectest"
)

// 本文件锁住 core#81 的 conf 侧三条语义：
// 解码钩子产出混淆持有器（不落明文）/ JSON wire 真值往返 / env 叶子扩展点。

const secretMarker = "oidc-secret-marker-9f2b"

func mapstructureDecode(t *testing.T, in map[string]any, out any) {
	t.Helper()
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		WeaklyTypedInput: true,
		DecodeHook:       SecretStringDecodeHook(),
		Result:           out,
	})
	require.NoError(t, err)
	require.NoError(t, dec.Decode(in))
}

// TestSecretStringDecodeHook_ConvertAndIsolate 钩子命中判定：
// string → *SecureString 转持有器（混淆存储不含明文、取回是原文）；
// 普通 string 字段原样解码，不受钩子影响（钩子只对 SecureString 目标生效）。
// 逗号切片归 viper 默认钩子（消费方必须组合），由 bootstrap 集成测试断言。
func TestSecretStringDecodeHook_ConvertAndIsolate(t *testing.T) {
	var out struct {
		Secret *secretx.SecureString `mapstructure:"secret"`
		Plain  string                `mapstructure:"plain"`
	}
	mapstructureDecode(t, map[string]any{
		"secret": secretMarker,
		"plain":  secretMarker,
	}, &out)

	require.NotNil(t, out.Secret, "钩子必须产出持有器")
	// 对照：普通 string 字段必须被扫到——否则本条只能证明扫描手法坏掉（假绿）。
	if !bytes.Contains([]byte(out.Plain), []byte(secretMarker)) {
		t.Fatal("对照失败：普通 string 里扫不到明文，扫描手法本身有问题")
	}
	sectest.AssertNotStoredInPlaintext(t, out.Secret, secretMarker)
	assert.Equal(t, secretMarker, out.Plain, "普通 string 字段必须原样解码")
}

// TestSecretStringDecodeHook_EmptyAndAbsent 空串产出空持有器（未配置语义）；
// 键缺席时字段保持 nil（mapstructure 不为缺席键调用钩子）——读取点两者都要容。
func TestSecretStringDecodeHook_EmptyAndAbsent(t *testing.T) {
	var out struct {
		Secret *secretx.SecureString `mapstructure:"secret"`
	}
	mapstructureDecode(t, map[string]any{"secret": ""}, &out)
	require.NotNil(t, out.Secret)
	assert.Empty(t, out.Secret.Get())

	var absent struct {
		Secret *secretx.SecureString `mapstructure:"secret"`
	}
	mapstructureDecode(t, map[string]any{}, &absent)
	assert.Nil(t, absent.Secret, "键缺席不得凭空建持有器")
}

// TestSecretStringDecodeHook_OtherTypesPassThrough 非字符串源不被钩子拦截，
// 走 mapstructure 原有路径（WeaklyTypedInput 的 "7" → 7）。
func TestSecretStringDecodeHook_OtherTypesPassThrough(t *testing.T) {
	var out struct {
		Skipped int `mapstructure:"skipped"`
	}
	mapstructureDecode(t, map[string]any{"skipped": "7"}, &out)
	assert.Equal(t, 7, out.Skipped)
}

// TestOIDCConfig_JSONRoundTrip 落库/字段级合并底座的真值往返：
// marshal 吐真值（明文在场——这是持久化正确性，不是泄漏），
// unmarshal 重建持有器且同样混淆存储、取回原文。
func TestOIDCConfig_JSONRoundTrip(t *testing.T) {
	cfg := OIDCConfig{
		Enabled:      true,
		Issuer:       "https://idp.example.com",
		ClientID:     "client-1",
		ClientSecret: NewSecretString(secretMarker),
		Scopes:       "openid profile email",
	}
	b, err := json.Marshal(cfg)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Equal(t, secretMarker, m["client_secret"], "落库 wire 必须真值（持久化往返）")
	assert.Equal(t, "client-1", m["client_id"])

	var back OIDCConfig
	require.NoError(t, json.Unmarshal(b, &back))
	require.NotNil(t, back.ClientSecret)
	sectest.AssertNotStoredInPlaintext(t, back.ClientSecret, secretMarker)
	assert.Equal(t, cfg.Issuer, back.Issuer)
	assert.Equal(t, cfg.Scopes, back.Scopes)
	assert.Equal(t, cfg.FrontendCallback, back.FrontendCallback)
}

// TestOIDCConfig_JSONUnsetShape 未配置的 wire 形态：nil 持有器 marshal 出空串；
// "client_secret": null / "" unmarshal 出空持有器（Get==""），不 panic。
func TestOIDCConfig_JSONUnsetShape(t *testing.T) {
	b, err := json.Marshal(OIDCConfig{ClientID: "c"})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Equal(t, "", m["client_secret"])

	var back OIDCConfig
	require.NoError(t, json.Unmarshal([]byte(`{"client_secret":null}`), &back))
	require.NotNil(t, back.ClientSecret)
	assert.Empty(t, back.ClientSecret.Get())

	var backEmpty OIDCConfig
	require.NoError(t, json.Unmarshal([]byte(`{"client_secret":""}`), &backEmpty))
	require.NotNil(t, backEmpty.ClientSecret)
	assert.Empty(t, backEmpty.ClientSecret.Get())
}

// TestSetSecretStringLeaf env 重施叶子扩展点：持有器形态整只替换（env 恒覆盖），
// 旧持有器不受影响（是否 Clear 由替换方决定——overlay 路径有 identity 守卫）；
// 非 SecureString 叶子不动返回 false。
func TestSetSecretStringLeaf(t *testing.T) {
	cfg := OIDCConfig{ClientSecret: NewSecretString("old-value")}
	old := cfg.ClientSecret
	field := reflect.ValueOf(&cfg).Elem().FieldByName("ClientSecret")
	require.True(t, SetSecretStringLeaf(field, secretMarker))
	sectest.AssertNotStoredInPlaintext(t, cfg.ClientSecret, secretMarker)
	assert.Equal(t, "old-value", old.Get(), "整只替换不得动旧持有器内容")

	// nil 持有器同样可写（env 恒覆盖对未配置叶子也生效）。
	var unset OIDCConfig
	f2 := reflect.ValueOf(&unset).Elem().FieldByName("ClientSecret")
	require.True(t, SetSecretStringLeaf(f2, secretMarker))
	require.NotNil(t, unset.ClientSecret)
	assert.Equal(t, secretMarker, unset.ClientSecret.Get())

	// 非 SecureString 叶子：不动、返回 false（走调用方原类型分派）。
	plain := OIDCConfig{Issuer: "x"}
	f3 := reflect.ValueOf(&plain).Elem().FieldByName("Issuer")
	assert.False(t, SetSecretStringLeaf(f3, "y"))
	assert.Equal(t, "x", plain.Issuer)
}
