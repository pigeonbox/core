// sanitize.go 管理台读边界脱敏（core#81）：SystemConfig 序列化给管理面前，
// 把敏感持有器换成掩码哨兵——响应体不吐明文。
package config

import (
	"github.com/pigeonbox/core/conf"
)

// SanitizeForAdmin 返回管理台读侧的**脱敏副本**：oidc.client_secret 换成
// conf.SecretMask（已配置）/ 空串（未配置），其余段浅拷贝共享（响应只读
// 序列化，不改写）。nil 入 nil 出。
//
// 为什么掩码在门面副本上而不是 OIDCConfig.MarshalJSON 里：落库（system_configs
// 单行 JSON）与 UpdateConfigJSON 的字段级合并底座都需要**真值**往返——在
// marshal 层掩码会让密钥活不过一次重启。明文的出域面只有管理台读响应这一个，
// 脱敏钉在门面（app/admin.GetConfig），内部消费方（装配/联邦同步/能力位）
// 直用本域 GetConfig 拿真值。
//
// 负对照判据（issue 验收第 2 条）：原始 SystemConfig 直接序列化必然扫得到
// 明文（conf/secure_test.go 与本域 sanitize_test.go 各有一条对照断言）——
// 改回明文实现 ⇒ 响应体扫得到，测试当场转红。
func SanitizeForAdmin(cfg *SystemConfig) *SystemConfig {
	if cfg == nil {
		return nil
	}
	out := *cfg
	if cfg.OIDC != nil {
		o := *cfg.OIDC
		if o.ClientSecret != nil && o.ClientSecret.Get() != "" {
			o.ClientSecret = conf.NewSecretString(conf.SecretMask)
		}
		out.OIDC = &o
	}
	return &out
}
