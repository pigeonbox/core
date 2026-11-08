// secure.go 敏感配置项的持有原语接线（core#81）。
//
// core#73 的试点结论：只把「消费方内部」换成 SecureString 而配置解码仍产
// 裸 string，等于明文从一份变两份。本文件把缺的那半补上：
//   - 解码：SecretStringDecodeHook 让 mapstructure/viper 解码器**直接填**
//     *SecureString 字段（bootstrap.InitConfig 传入）——明文自解码起就不在
//     配置结构体里驻留；
//   - JSON 往返：OIDCConfig 的 wire 仍是字符串（system_configs 落库、
//     UpdateConfigJSON 字段级合并底座都要真值往返；管理台**读响应**的脱敏
//     不在这一层，在 app/admin 门面 SanitizeForAdmin）；
//   - env 重施：SetSecretStringLeaf 让 bootstrap.ReapplyEnvOverrides 能把
//     env 值写回持有器形态的叶子（env 恒覆盖规则对 client_secret 不失效）。
package conf

import (
	"encoding/json"
	"reflect"

	mapstructure "github.com/go-viper/mapstructure/v2"
	"github.com/oekit/pkg/secretx"
)

// SecretMask 管理台读回 client_secret 的掩码哨兵（定长，不随真值长度变化——
// 长度也是信息）。
//
// 三态约定（core#81）：
//   - 读（app/admin 门面 SanitizeForAdmin）：已配置 ⇒ 掩码；未配置 ⇒ 空串
//     （前端密码框可区分两态）；
//   - 写（app/config.UpdateConfig）：提交值 == 掩码 ⇒ 必然是表单原样回传
//     的「未改动」，保留现值——把掩码当新密钥落库会让 OIDC 登录当场失效；
//   - 取舍：真值恰为 "********" 的管理员无法再经管理台改它（需先换成别的
//     值）——比「掩码落库」「读侧吐明文」都好的取舍，如实写进文档。
const SecretMask = "********"

// secureStringType secretx.SecureString 的 reflect.Type（钩子/env 叶子判型用）。
var secureStringType = reflect.TypeOf(secretx.SecureString{})

// NewSecretString 构造持有器（配置装配点的统一入口；空串 = 未配置语义，
// 持有器内部零化）。
func NewSecretString(plaintext string) *secretx.SecureString {
	return secretx.NewSecureString(plaintext)
}

// SecretStringDecodeHook mapstructure 解码钩子：**string 源 → SecureString 目标**
// 时转成持有器（XOR 混淆存储），其余类型/字段原样放行。
//
// 判据是「目标**元素类型**是 SecureString」：字段声明为 *SecureString 时钩子在
// 字段层先命中（to == *SecureString），decodePtr 解引用后的内层解码再次调用
// （from = *SecureString）会被 from.Kind() != String 挡住原样放行——钩子只转一次。
// 普通 string 字段（issuer/client_id 等）与其它类型完全不受影响。
//
// ⚠️ 组合注意：viper.DecodeHook 选项会**整体覆盖** viper 的默认钩子组合
// （duration / 逗号切片，v1.21 defaultDecoderConfig 先设默认后应用 opts）——
// 消费方（bootstrap.InitConfig）必须把默认钩子一并 ComposeDecodeHookFunc
// 组合进来，否则既有字段解码行为被静默改变。
func SecretStringDecodeHook() mapstructure.DecodeHookFunc {
	return func(from reflect.Type, to reflect.Type, data any) (any, error) {
		if from == nil || from.Kind() != reflect.String {
			return data, nil
		}
		target := to
		if target != nil && target.Kind() == reflect.Pointer {
			target = target.Elem()
		}
		if target != secureStringType {
			return data, nil
		}
		// data 可能是具名 string 类型（viper getSettings 产普通 string，防御
		// 具名形态），经 reflect 取值而非类型断言。
		s := reflect.ValueOf(data).String()
		return secretx.NewSecureString(s), nil
	}
}

// SetSecretStringLeaf env 重施器（bootstrap.ReapplyEnvOverrides）的叶子扩展点：
// field 形态为 *secretx.SecureString 时以 raw 明文重建持有器**整只替换**（env
// 恒覆盖 = env 值生效；nil/非 nil 持有器都不影响），返回 true；其他形态不动、
// 返回 false（调用方按原类型分派）。setConfLeaf 的 string 分派对 struct 叶子
// 不识别，没有这个扩展点 PB_OIDC_CLIENT_SECRET 的 env 恒覆盖会被静默跳过。
func SetSecretStringLeaf(field reflect.Value, raw string) bool {
	if field.Kind() != reflect.Pointer || field.Type().Elem() != secureStringType {
		return false
	}
	field.Set(reflect.ValueOf(secretx.NewSecureString(raw)))
	return true
}

// oidcConfigWire OIDCConfig 的 JSON 线形状（client_secret 为字符串）。
type oidcConfigWire struct {
	Enabled          bool   `json:"enabled"`
	Issuer           string `json:"issuer"`
	ClientID         string `json:"client_id"`
	ClientSecret     string `json:"client_secret"`
	Scopes           string `json:"scopes"`
	FrontendCallback string `json:"frontend_callback"`
}

// MarshalJSON wire 出：client_secret 吐**真值**——system_configs 落库与
// UpdateConfigJSON 的字段级合并底座都要真值往返（重启恢复、掩码哨兵回填、
// 值等复用都依赖它）。在 marshal 层掩码会让密钥活不过一次重启。
//
// 管理台**读响应**的脱敏不在这层：app/admin 门面 GetConfig 走
// SanitizeForAdmin 换掩码哨兵后序列化——明文的出域面只有管理台读响应一个，
// 脱敏钉在门面。
func (c OIDCConfig) MarshalJSON() ([]byte, error) {
	w := oidcConfigWire{
		Enabled:          c.Enabled,
		Issuer:           c.Issuer,
		ClientID:         c.ClientID,
		Scopes:           c.Scopes,
		FrontendCallback: c.FrontendCallback,
	}
	if c.ClientSecret != nil {
		w.ClientSecret = c.ClientSecret.Get()
	}
	return json.Marshal(w)
}

// UnmarshalJSON wire 入：字符串 → 持有器（每次解码建新持有器；nil/显式 null
// 视为「未配置」= 空持有器）。旧持有器是否 Clear 由替换方决定——overlay 路径
// 有 identity 守卫（app/config.applySystemConfigOverlayLocked），同指针不清。
func (c *OIDCConfig) UnmarshalJSON(b []byte) error {
	var w oidcConfigWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*c = OIDCConfig{
		Enabled:          w.Enabled,
		Issuer:           w.Issuer,
		ClientID:         w.ClientID,
		ClientSecret:     secretx.NewSecureString(w.ClientSecret),
		Scopes:           w.Scopes,
		FrontendCallback: w.FrontendCallback,
	}
	return nil
}
