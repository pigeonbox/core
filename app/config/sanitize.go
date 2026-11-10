// sanitize.go 管理台读边界脱敏（core#81；core#108 扩到全部凭据字段）：
// SystemConfig 序列化给管理面前，把敏感字段换成掩码哨兵——响应体不吐明文；
// 写侧配对函数 restoreMaskedSecrets 把「掩码哨兵回传」还原成现值（= 未改）。
package config

import (
	"fmt"

	"github.com/pigeonbox/core/conf"
)

// SanitizeForAdmin 返回管理台读侧的**脱敏副本**，凭据字段换成
// conf.SecretMask（已配置）/ 空串（未配置）；被改写的段做**逐段深拷贝**，
// 不与入参共享指针（否则掩码会写进活配置）。nil 入 nil 出。
//
// 脱敏字段清单（core#81 起 OIDC client_secret；core#108 补齐全量——同一响应里
// 一半凭据掩码、一半明文回显，等于没掩）：
//
//	oidc.client_secret（*SecureString 持有器）
//	notify.smtp.password / notify.telegram.bot_token / notify.bark.device_key
//	runtime_storage 各后端凭据（webdav/ftp/sftp/azureblob/onedrive/s3 及
//	  oss/cos/bos/ks3/obs 的 secret_key；sftp 另含 private_key）
//	p2p.federation.delivery_peers[].token（共享口令）
//
// 为什么掩码在门面副本上而不是 MarshalJSON 里：落库（system_configs
// 单行 JSON）与 UpdateConfigJSON 的字段级合并底座都需要**真值**往返——在
// marshal 层掩码会让密钥活不过一次重启。明文的出域面只有管理台读响应这一个，
// 脱敏钉在门面（app/admin.GetConfig），内部消费方（装配/联邦同步/能力位）
// 直用本域 GetConfig 拿真值。
//
// 写侧闭环（core#108）：管理台表单把掩码原样回传 ⇒ UpdateConfig 走
// restoreMaskedSecrets「哨兵 = 未改」回填现值——掩码不当新凭据落库。
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
	// Notify 段：SMTP/Telegram/Bark 是值子段，整段拷贝后改副本即可隔离。
	if cfg.Notify != nil {
		n := *cfg.Notify
		maskSecret(&n.SMTP.Password)
		maskSecret(&n.Telegram.BotToken)
		maskSecret(&n.Bark.DeviceKey)
		out.Notify = &n
	}
	// 运行时存储段：子配置都是指针，逐个拷贝后再掩（防改到活配置）。
	if cfg.RuntimeStorage != nil {
		out.RuntimeStorage = maskStorageSecrets(cfg.RuntimeStorage)
	}
	// P2P 联邦段：对端口令掩码；DeliveryPeers 切片同样要拷贝。
	if cfg.P2P != nil {
		p := *cfg.P2P
		if cfg.P2P.Federation != nil {
			f := *cfg.P2P.Federation
			peers := make([]conf.FederationDeliveryPeer, len(f.DeliveryPeers))
			copy(peers, f.DeliveryPeers)
			for i := range peers {
				maskSecret(&peers[i].Token)
			}
			f.DeliveryPeers = peers
			p.Federation = &f
		}
		out.P2P = &p
	}
	return &out
}

// maskSecret 凭据三态（同 OIDC）：已配置 ⇒ 掩码；未配置（空串）⇒ 保持空串
// （前端密码框可区分「已配置/未配置」两态）。
func maskSecret(v *string) {
	if v != nil && *v != "" {
		*v = conf.SecretMask
	}
}

// maskStorageSecrets 存储后端凭据脱敏。返回**逐段深拷贝**（只深拷贝被改写的
// 子段；HDFS 无凭据字段，指针共享、响应只读）。
func maskStorageSecrets(src *conf.StorageConfig) *conf.StorageConfig {
	rs := *src
	if src.S3 != nil {
		s := *src.S3
		maskSecret(&s.SecretKey)
		rs.S3 = &s
	}
	if src.WebDAV != nil {
		w := *src.WebDAV
		maskSecret(&w.Password)
		rs.WebDAV = &w
	}
	if src.FTP != nil {
		f := *src.FTP
		maskSecret(&f.Password)
		rs.FTP = &f
	}
	if src.SFTP != nil {
		s := *src.SFTP
		maskSecret(&s.Password)
		maskSecret(&s.PrivateKey)
		rs.SFTP = &s
	}
	if src.AzureBlob != nil {
		a := *src.AzureBlob
		maskSecret(&a.Key)
		maskSecret(&a.SAS)
		rs.AzureBlob = &a
	}
	if src.OneDrive != nil {
		o := *src.OneDrive
		maskSecret(&o.ClientSecret)
		maskSecret(&o.RefreshToken)
		rs.OneDrive = &o
	}
	for _, c := range []**conf.CloudStorageConfig{&rs.OSS, &rs.COS, &rs.BOS, &rs.KS3, &rs.OBS} {
		if *c == nil {
			continue
		}
		cc := *(*c)
		maskSecret(&cc.SecretKey)
		*c = &cc
	}
	return &rs
}

// restoreMaskedSecrets 写侧掩码哨兵回填（core#108）：提交值 == conf.SecretMask
// ⇒ 管理台表单原样回传的「未改动」⇒ 回填**现值**；其余取值原样放行（= 显式改值）。
// 在 UpdateConfig 校验/落库**之前**调用——校验与持久化看到的必须是回填后的值。
//
// 语义与 OIDC 写侧守卫（core#81，UpdateConfig 内）同款；区别是本函数覆盖
// 全部**明文 string** 凭据字段（OIDC 是持有器，走 identity 守卫，不在此列）。
//
// 对端口令是列表字段，「未改」按**URL 对端身份**匹配：表单回传顺序可变，
// URL 是对端的稳定标识。匹配不到（新增对端/改了 URL 且只回传了掩码）⇒
// 显式报错让管理员重填口令——掩码落库会把对端口令静默换成 "********"。
func restoreMaskedSecrets(in, cur *SystemConfig) error {
	if in == nil {
		return nil
	}
	if in.Notify != nil {
		var curN conf.NotifyConfig
		if cur != nil && cur.Notify != nil {
			curN = *cur.Notify
		}
		restoreMasked(&in.Notify.SMTP.Password, curN.SMTP.Password)
		restoreMasked(&in.Notify.Telegram.BotToken, curN.Telegram.BotToken)
		restoreMasked(&in.Notify.Bark.DeviceKey, curN.Bark.DeviceKey)
	}
	if in.RuntimeStorage != nil {
		var curS conf.StorageConfig
		if cur != nil && cur.RuntimeStorage != nil {
			curS = *cur.RuntimeStorage
		}
		restoreStorageMasked(in.RuntimeStorage, &curS)
	}
	if in.P2P != nil && in.P2P.Federation != nil {
		var curPeers []conf.FederationDeliveryPeer
		if cur != nil && cur.P2P != nil && cur.P2P.Federation != nil {
			curPeers = cur.P2P.Federation.DeliveryPeers
		}
		for i := range in.P2P.Federation.DeliveryPeers {
			p := &in.P2P.Federation.DeliveryPeers[i]
			if p.Token != conf.SecretMask {
				continue
			}
			tok, ok := lookupPeerToken(curPeers, p.URL)
			if !ok {
				return fmt.Errorf("p2p.federation.delivery_peers[%d].token 是掩码哨兵但匹配不到现值（新增对端或 URL 已变更）：请重新填写口令", i)
			}
			p.Token = tok
		}
	}
	return nil
}

// restoreMasked 单字段哨兵回填：== 掩码 ⇒ 换现值（现值可为空串 = 本就未配置）。
func restoreMasked(v *string, cur string) {
	if v != nil && *v == conf.SecretMask {
		*v = cur
	}
}

// restoreStorageMasked 存储后端凭据的哨兵回填（子段指针可为 nil = 未提交）。
func restoreStorageMasked(in, cur *conf.StorageConfig) {
	if in.S3 != nil && cur.S3 != nil {
		restoreMasked(&in.S3.SecretKey, cur.S3.SecretKey)
	}
	if in.WebDAV != nil && cur.WebDAV != nil {
		restoreMasked(&in.WebDAV.Password, cur.WebDAV.Password)
	}
	if in.FTP != nil && cur.FTP != nil {
		restoreMasked(&in.FTP.Password, cur.FTP.Password)
	}
	if in.SFTP != nil && cur.SFTP != nil {
		restoreMasked(&in.SFTP.Password, cur.SFTP.Password)
		restoreMasked(&in.SFTP.PrivateKey, cur.SFTP.PrivateKey)
	}
	if in.AzureBlob != nil && cur.AzureBlob != nil {
		restoreMasked(&in.AzureBlob.Key, cur.AzureBlob.Key)
		restoreMasked(&in.AzureBlob.SAS, cur.AzureBlob.SAS)
	}
	if in.OneDrive != nil && cur.OneDrive != nil {
		restoreMasked(&in.OneDrive.ClientSecret, cur.OneDrive.ClientSecret)
		restoreMasked(&in.OneDrive.RefreshToken, cur.OneDrive.RefreshToken)
	}
	for _, pair := range []struct{ in, cur *conf.CloudStorageConfig }{
		{in.OSS, cur.OSS}, {in.COS, cur.COS}, {in.BOS, cur.BOS},
		{in.KS3, cur.KS3}, {in.OBS, cur.OBS},
	} {
		if pair.in != nil && pair.cur != nil {
			restoreMasked(&pair.in.SecretKey, pair.cur.SecretKey)
		}
	}
}

// lookupPeerToken 按 URL（对端身份）在现值列表里找口令。
func lookupPeerToken(peers []conf.FederationDeliveryPeer, url string) (string, bool) {
	for _, p := range peers {
		if p.URL == url {
			return p.Token, true
		}
	}
	return "", false
}
