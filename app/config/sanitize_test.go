package config

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/internal/sectest"
)

// 本文件锁住 core#81 的管理台读写两条链：
// 读响应不吐明文（负对照：原始序列化必然扫得到）/ 写侧掩码哨兵回填与
// 值等复用 / 热更旋转旧持有器零化。

const sanitizeMarker = "admin-leak-marker-4e9a"

func seedGlobalConf(t *testing.T) {
	t.Helper()
	old := conf.Snapshot()
	conf.SetGlobalConfig(&conf.AppConfiguration{})
	t.Cleanup(func() { conf.SetGlobalConfig(old) })
}

// TestSanitizeForAdmin_NoPlaintext 管理台读响应体不含明文（issue 验收第 2 条）。
// 负对照内嵌：**未经脱敏**的原始 SystemConfig 直接序列化必须扫得到明文——
// 证明扫描手法有效、脱敏边界有牙齿（改回明文实现 ⇒ 本条当场转红）。
func TestSanitizeForAdmin_NoPlaintext(t *testing.T) {
	cfg := &SystemConfig{
		OIDC: &conf.OIDCConfig{
			Enabled:      true,
			Issuer:       "https://idp.example.com",
			ClientID:     "client-1",
			ClientSecret: conf.NewSecretString(sanitizeMarker),
		},
	}

	// 负对照：原始序列化（= 落库 wire 形状）扫得到明文。
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	if !bytes.Contains(raw, []byte(sanitizeMarker)) {
		t.Fatal("负对照失败：原始序列化扫不到明文，扫描手法本身有问题")
	}

	got := SanitizeForAdmin(cfg)
	body, err := json.Marshal(got)
	require.NoError(t, err)
	if bytes.Contains(body, []byte(sanitizeMarker)) {
		t.Fatal("管理台读响应泄漏明文 client_secret")
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	oidc := m["oidc"].(map[string]any)
	assert.Equal(t, conf.SecretMask, oidc["client_secret"], "已配置 ⇒ 掩码哨兵")
	assert.Equal(t, "client-1", oidc["client_id"], "非敏感字段原样")

	// 原始配置不受脱敏影响（真值仍在，供落库/热更）。
	assert.Equal(t, sanitizeMarker, cfg.OIDC.ClientSecret.Get())
}

// TestSanitizeForAdmin_UnsetSecret 未配置（nil / 空持有器）回空串而非掩码——
// 前端密码框可区分「已配置/未配置」两态。
func TestSanitizeForAdmin_UnsetSecret(t *testing.T) {
	for name, cfg := range map[string]*SystemConfig{
		"nil_holder": {OIDC: &conf.OIDCConfig{}},
		"empty":      {OIDC: &conf.OIDCConfig{ClientSecret: conf.NewSecretString("")}},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(SanitizeForAdmin(cfg))
			require.NoError(t, err)
			var m map[string]any
			require.NoError(t, json.Unmarshal(body, &m))
			assert.Equal(t, "", m["oidc"].(map[string]any)["client_secret"])
		})
	}
	assert.Nil(t, SanitizeForAdmin(nil), "nil 入 nil 出")
}

// TestUpdateConfig_MaskSentinelPreservesSecret 掩码哨兵回填：提交值 == 掩码
// ⇒ 保留现值（identity 与内容都不变），掩码不落库、不进 conf。
func TestUpdateConfig_MaskSentinelPreservesSecret(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	seedGlobalConf(t)

	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		OIDC: &conf.OIDCConfig{Enabled: true, ClientSecret: conf.NewSecretString(sanitizeMarker)},
	}))
	live := conf.Snapshot().Security.OIDC.ClientSecret
	require.Equal(t, sanitizeMarker, live.Get())

	// 管理台表单原样回传掩码 → 保存 → 现值原样保留（同持有器）。
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		OIDC: &conf.OIDCConfig{Enabled: true, ClientSecret: conf.NewSecretString(conf.SecretMask)},
	}))
	assert.Same(t, live, conf.Snapshot().Security.OIDC.ClientSecret, "哨兵回填必须复用现持有器")
	assert.Equal(t, sanitizeMarker, conf.Snapshot().Security.OIDC.ClientSecret.Get())
	if bytes.Contains(sectest.RawObfuscated(t, conf.Snapshot().Security.OIDC.ClientSecret), []byte(conf.SecretMask)) {
		t.Fatal("掩码被当成新密钥存进了持有器")
	}

	// 落库记录同样保留真值（掩码不落库）。
	persisted, err := svc.loadPersistedConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, persisted.OIDC)
	assert.Equal(t, sanitizeMarker, persisted.OIDC.ClientSecret.Get())
}

// TestUpdateConfig_RotateSecretClearsOld 热更旋转：conf 换新值，旧持有器
// 混淆存储零化（反射断言，issue 验收第 4 条）。
func TestUpdateConfig_RotateSecretClearsOld(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	seedGlobalConf(t)

	old := conf.NewSecretString("rotate-old-2b71")
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		OIDC: &conf.OIDCConfig{Enabled: true, ClientSecret: old},
	}))
	require.Equal(t, "rotate-old-2b71", conf.Snapshot().Security.OIDC.ClientSecret.Get())

	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		OIDC: &conf.OIDCConfig{Enabled: true, ClientSecret: conf.NewSecretString("rotate-new-9d44")},
	}))
	cur := conf.Snapshot().Security.OIDC.ClientSecret
	require.NotNil(t, cur)
	assert.Equal(t, "rotate-new-9d44", cur.Get(), "conf 即刻生效新值")
	assert.NotSame(t, old, cur)
	sectest.AssertCleared(t, old)

	// 镜像（管理台回读底座）同步为新值；落库为提交意图（新明文）。
	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, "rotate-new-9d44", got.OIDC.ClientSecret.Get())
	persisted, err := svc.loadPersistedConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, "rotate-new-9d44", persisted.OIDC.ClientSecret.Get())
}

// TestUpdateConfig_EqualValueReusesHolder 值等复用：提交与现值明文相同的
// 保存复用现持有器（identity 稳定 ⇒ overlay 守卫跳过 Clear ⇒ 未改 secret 的
// 保存不零化现役密钥、不毒化在途请求）。
func TestUpdateConfig_EqualValueReusesHolder(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	seedGlobalConf(t)

	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		OIDC: &conf.OIDCConfig{Enabled: true, ClientSecret: conf.NewSecretString(sanitizeMarker)},
	}))
	live := conf.Snapshot().Security.OIDC.ClientSecret

	// 值相同（新持有器、同明文）→ 复用现持有器。
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		OIDC: &conf.OIDCConfig{Enabled: true, ClientSecret: conf.NewSecretString(sanitizeMarker)},
	}))
	assert.Same(t, live, conf.Snapshot().Security.OIDC.ClientSecret)
	sectest.AssertNotStoredInPlaintext(t, conf.Snapshot().Security.OIDC.ClientSecret, sanitizeMarker)
}

// TestUpdateConfigOIDCSectionAbsentKeepsSecret 携带其它段的局部保存不冲掉
// 已配置的 client_secret（nil 段保留 + 持有器随 conf 原样）。
func TestUpdateConfigOIDCSectionAbsentKeepsSecret(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	seedGlobalConf(t)

	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		OIDC: &conf.OIDCConfig{Enabled: true, ClientSecret: conf.NewSecretString(sanitizeMarker)},
	}))
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		Base: &BaseConfig{Name: "renamed", Description: "d"},
	}))
	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.OIDC)
	require.NotNil(t, got.OIDC.ClientSecret)
	assert.True(t, got.OIDC.Enabled)
	assert.Equal(t, sanitizeMarker, got.OIDC.ClientSecret.Get())
}

// ==================== core#108：凭据掩码扩到全量字段 ====================

// coreCredMarker 各凭据字段的植入明文（读侧扫描判据的探针）。
var coreCredMarkers = map[string]string{
	"notify.smtp.password":                   "mk-smtp-1c7f",
	"notify.telegram.bot_token":              "mk-tg-2d8e",
	"notify.bark.device_key":                 "mk-bark-3e9a",
	"runtime_storage.webdav.password":        "mk-webdav-4f0b",
	"runtime_storage.ftp.password":           "mk-ftp-5a1c",
	"runtime_storage.sftp.password":          "mk-sftp-6b2d",
	"runtime_storage.sftp.private_key":       "mk-sftpkey-7c3e",
	"runtime_storage.azureblob.key":          "mk-azkey-8d4f",
	"runtime_storage.azureblob.sas":          "mk-azsas-9e5a",
	"runtime_storage.onedrive.client_secret": "mk-odcs-0f6b",
	"runtime_storage.onedrive.refresh_token": "mk-odrt-1a7c",
	"runtime_storage.s3.secret_key":          "mk-s3-2b8d",
	"runtime_storage.oss.secret_key":         "mk-oss-3c9e",
	"runtime_storage.cos.secret_key":         "mk-cos-4d0f",
	"runtime_storage.bos.secret_key":         "mk-bos-5e1a",
	"runtime_storage.ks3.secret_key":         "mk-ks3-6f2b",
	"runtime_storage.obs.secret_key":         "mk-obs-7a3c",
	"p2p.federation.delivery_peers[0].token": "mk-peer-8b4d",
}

// plantAllCredentials 构造「全部凭据字段都埋了明文」的 SystemConfig，
// 同时放几个**非敏感**邻居字段（掩码不得误伤）。
func plantAllCredentials() *SystemConfig {
	return &SystemConfig{
		Notify: &conf.NotifyConfig{
			SMTP:     conf.SMTPConfig{Host: "smtp.example.com", Username: "mailer", Password: coreCredMarkers["notify.smtp.password"]},
			Telegram: conf.TelegramConfig{BotToken: coreCredMarkers["notify.telegram.bot_token"], ChatID: "chat-1"},
			Bark:     conf.BarkConfig{ServerURL: "https://bark.example.com", DeviceKey: coreCredMarkers["notify.bark.device_key"]},
		},
		RuntimeStorage: &conf.StorageConfig{
			Type:      "s3",
			S3:        &conf.S3Config{Endpoint: "https://s3.example.com", AccessKey: "AK-visible", SecretKey: coreCredMarkers["runtime_storage.s3.secret_key"]},
			WebDAV:    &conf.WebDAVConfig{Endpoint: "https://dav.example.com", Username: "dav-user", Password: coreCredMarkers["runtime_storage.webdav.password"]},
			FTP:       &conf.FTPConfig{Host: "ftp.example.com", Username: "ftp-user", Password: coreCredMarkers["runtime_storage.ftp.password"]},
			SFTP:      &conf.SFTPConfig{Host: "sftp.example.com", Username: "sftp-user", Password: coreCredMarkers["runtime_storage.sftp.password"], PrivateKey: coreCredMarkers["runtime_storage.sftp.private_key"]},
			AzureBlob: &conf.AzureBlobConfig{Account: "acct", Container: "box", Key: coreCredMarkers["runtime_storage.azureblob.key"], SAS: coreCredMarkers["runtime_storage.azureblob.sas"]},
			OneDrive:  &conf.OneDriveConfig{ClientID: "od-client", ClientSecret: coreCredMarkers["runtime_storage.onedrive.client_secret"], RefreshToken: coreCredMarkers["runtime_storage.onedrive.refresh_token"]},
			OSS:       &conf.CloudStorageConfig{Bucket: "b", AccessKey: "AK-oss-visible", SecretKey: coreCredMarkers["runtime_storage.oss.secret_key"]},
			COS:       &conf.CloudStorageConfig{Bucket: "b", SecretKey: coreCredMarkers["runtime_storage.cos.secret_key"]},
			BOS:       &conf.CloudStorageConfig{Bucket: "b", SecretKey: coreCredMarkers["runtime_storage.bos.secret_key"]},
			KS3:       &conf.CloudStorageConfig{Bucket: "b", SecretKey: coreCredMarkers["runtime_storage.ks3.secret_key"]},
			OBS:       &conf.CloudStorageConfig{Bucket: "b", SecretKey: coreCredMarkers["runtime_storage.obs.secret_key"]},
		},
		P2P: &P2PSection{Federation: &conf.FederationConfig{
			DeliveryPeers: []conf.FederationDeliveryPeer{{
				URL: "https://peer-a.example.com", Token: coreCredMarkers["p2p.federation.delivery_peers[0].token"], Name: "张三的柜子",
			}},
		}},
	}
}

// TestSanitizeForAdmin_MasksAllCredentials core#108 读侧：管理台读响应不得回显
// 任何凭据明文（此前只掩 OIDC client_secret ⇒ 同一响应一半掩码一半明文）。
//
// 负对照内嵌：**未经脱敏**的原始序列化必须逐个扫得到植入明文——证明扫描手法
// 对每个字段都有效（改回明文实现 ⇒ 本条当场转红）。
func TestSanitizeForAdmin_MasksAllCredentials(t *testing.T) {
	cfg := plantAllCredentials()

	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	for path, marker := range coreCredMarkers {
		if !bytes.Contains(raw, []byte(marker)) {
			t.Fatalf("负对照失败：%s 的明文在原始序列化里扫不到，扫描手法对该字段无效", path)
		}
	}

	got := SanitizeForAdmin(cfg)
	body, err := json.Marshal(got)
	require.NoError(t, err)
	for path, marker := range coreCredMarkers {
		if bytes.Contains(body, []byte(marker)) {
			t.Fatalf("管理台读响应泄漏明文：%s", path)
		}
	}

	// 逐字段断言掩码哨兵（已配置 ⇒ 掩码）+ 非敏感邻居原样
	assert.Equal(t, conf.SecretMask, got.Notify.SMTP.Password)
	assert.Equal(t, conf.SecretMask, got.Notify.Telegram.BotToken)
	assert.Equal(t, conf.SecretMask, got.Notify.Bark.DeviceKey)
	assert.Equal(t, "mailer", got.Notify.SMTP.Username, "用户名非凭据不得误伤")
	assert.Equal(t, "chat-1", got.Notify.Telegram.ChatID)
	assert.Equal(t, conf.SecretMask, got.RuntimeStorage.S3.SecretKey)
	assert.Equal(t, "AK-visible", got.RuntimeStorage.S3.AccessKey, "AccessKey 显示口径同 OIDC client_id")
	assert.Equal(t, conf.SecretMask, got.RuntimeStorage.WebDAV.Password)
	assert.Equal(t, conf.SecretMask, got.RuntimeStorage.FTP.Password)
	assert.Equal(t, conf.SecretMask, got.RuntimeStorage.SFTP.Password)
	assert.Equal(t, conf.SecretMask, got.RuntimeStorage.SFTP.PrivateKey)
	assert.Equal(t, conf.SecretMask, got.RuntimeStorage.AzureBlob.Key)
	assert.Equal(t, conf.SecretMask, got.RuntimeStorage.AzureBlob.SAS)
	assert.Equal(t, conf.SecretMask, got.RuntimeStorage.OneDrive.ClientSecret)
	assert.Equal(t, conf.SecretMask, got.RuntimeStorage.OneDrive.RefreshToken)
	for name, c := range map[string]*conf.CloudStorageConfig{
		"oss": got.RuntimeStorage.OSS, "cos": got.RuntimeStorage.COS,
		"bos": got.RuntimeStorage.BOS, "ks3": got.RuntimeStorage.KS3, "obs": got.RuntimeStorage.OBS,
	} {
		assert.Equal(t, conf.SecretMask, c.SecretKey, name)
	}
	assert.Equal(t, conf.SecretMask, got.P2P.Federation.DeliveryPeers[0].Token)
	assert.Equal(t, "https://peer-a.example.com", got.P2P.Federation.DeliveryPeers[0].URL, "对端 URL 非凭据")
	assert.Equal(t, "张三的柜子", got.P2P.Federation.DeliveryPeers[0].Name, "展示名非凭据")

	// 原始配置不受脱敏影响（真值仍在，供落库/热更）；且脱敏副本不共享切片/指针。
	assert.Equal(t, coreCredMarkers["notify.smtp.password"], cfg.Notify.SMTP.Password)
	assert.Equal(t, coreCredMarkers["p2p.federation.delivery_peers[0].token"], cfg.P2P.Federation.DeliveryPeers[0].Token)
	assert.NotSame(t, cfg.RuntimeStorage.S3, got.RuntimeStorage.S3, "凭据子段必须深拷贝")

	// 未配置（空串）⇒ 空串而非掩码（前端密码框可区分两态）。
	empty := SanitizeForAdmin(&SystemConfig{
		Notify:         &conf.NotifyConfig{},
		RuntimeStorage: &conf.StorageConfig{S3: &conf.S3Config{}},
		P2P:            &P2PSection{Federation: &conf.FederationConfig{DeliveryPeers: []conf.FederationDeliveryPeer{{URL: "https://p.example.com"}}}},
	})
	ebody, err := json.Marshal(empty)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(ebody, &m))
	assert.Equal(t, "", m["notify"].(map[string]any)["smtp"].(map[string]any)["password"])
	assert.Equal(t, "", m["p2p"].(map[string]any)["federation"].(map[string]any)["delivery_peers"].([]any)[0].(map[string]any)["token"])
}

// TestUpdateConfig_MaskSentinelRoundTrip core#108 写侧：读侧给掩码 ⇒ 表单原样
// 回传 ⇒ 「哨兵 = 未改」回填现值。以**完整回环**（读 → 脱敏 → 原样提交 → 再读）
// 模拟管理台保存；任一字段的真值被掩码覆盖 ⇒ 本条当场转红。
func TestUpdateConfig_MaskSentinelRoundTrip(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	seedGlobalConf(t)

	require.NoError(t, svc.UpdateConfig(ctx, plantAllCredentials()))

	// 管理台读 → 表单原样回传（含掩码）。
	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	form, err := json.Marshal(SanitizeForAdmin(got))
	require.NoError(t, err)
	require.NoError(t, svc.UpdateConfigJSON(ctx, form))

	// 全部凭据仍是真值（掩码不得落库）。
	got2, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	for path, marker := range coreCredMarkers {
		var v string
		switch path {
		case "notify.smtp.password":
			v = got2.Notify.SMTP.Password
		case "notify.telegram.bot_token":
			v = got2.Notify.Telegram.BotToken
		case "notify.bark.device_key":
			v = got2.Notify.Bark.DeviceKey
		case "runtime_storage.webdav.password":
			v = got2.RuntimeStorage.WebDAV.Password
		case "runtime_storage.ftp.password":
			v = got2.RuntimeStorage.FTP.Password
		case "runtime_storage.sftp.password":
			v = got2.RuntimeStorage.SFTP.Password
		case "runtime_storage.sftp.private_key":
			v = got2.RuntimeStorage.SFTP.PrivateKey
		case "runtime_storage.azureblob.key":
			v = got2.RuntimeStorage.AzureBlob.Key
		case "runtime_storage.azureblob.sas":
			v = got2.RuntimeStorage.AzureBlob.SAS
		case "runtime_storage.onedrive.client_secret":
			v = got2.RuntimeStorage.OneDrive.ClientSecret
		case "runtime_storage.onedrive.refresh_token":
			v = got2.RuntimeStorage.OneDrive.RefreshToken
		case "runtime_storage.s3.secret_key":
			v = got2.RuntimeStorage.S3.SecretKey
		case "runtime_storage.oss.secret_key":
			v = got2.RuntimeStorage.OSS.SecretKey
		case "runtime_storage.cos.secret_key":
			v = got2.RuntimeStorage.COS.SecretKey
		case "runtime_storage.bos.secret_key":
			v = got2.RuntimeStorage.BOS.SecretKey
		case "runtime_storage.ks3.secret_key":
			v = got2.RuntimeStorage.KS3.SecretKey
		case "runtime_storage.obs.secret_key":
			v = got2.RuntimeStorage.OBS.SecretKey
		case "p2p.federation.delivery_peers[0].token":
			v = got2.P2P.Federation.DeliveryPeers[0].Token
		}
		assert.Equal(t, marker, v, "%s 被掩码覆盖（哨兵回填失效）", path)
	}

	// 落库记录同样是真值（掩码不落库）。
	persisted, err := svc.loadPersistedConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, coreCredMarkers["notify.smtp.password"], persisted.Notify.SMTP.Password)
	assert.Equal(t, coreCredMarkers["p2p.federation.delivery_peers[0].token"], persisted.P2P.Federation.DeliveryPeers[0].Token)

	// 显式新值仍然生效（哨兵只拦「未改」，不拦真改动）。
	require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(`{"notify":{"smtp":{"password":"brand-new-9d44"}}}`)))
	got3, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, "brand-new-9d44", got3.Notify.SMTP.Password, "显式提交新值应生效")
}

// TestUpdateConfig_PeerTokenMaskByURL 对端口令的「未改」按 URL 身份匹配：
// 列表顺序变化不串口令；匹配不到（新增对端只回传掩码）⇒ 显式报错不落库。
func TestUpdateConfig_PeerTokenMaskByURL(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	seedGlobalConf(t)

	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{P2P: &P2PSection{Federation: &conf.FederationConfig{
		DeliveryPeers: []conf.FederationDeliveryPeer{
			{URL: "https://peer-a.example.com", Token: "tok-a", Name: "A"},
			{URL: "https://peer-b.example.com", Token: "tok-b", Name: "B"},
		},
	}}}))

	// 顺序调换 + 掩码回传 ⇒ 按 URL 还原，口令不串。
	require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(`{"p2p":{"federation":{"delivery_peers":[
		{"url":"https://peer-b.example.com","token":"********","name":"B"},
		{"url":"https://peer-a.example.com","token":"********","name":"A"}]}}}`)))
	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	peers := got.P2P.Federation.DeliveryPeers
	require.Len(t, peers, 2)
	assert.Equal(t, "tok-b", peers[0].Token, "按 URL 匹配：B 对端不得拿到 A 的口令")
	assert.Equal(t, "tok-a", peers[1].Token)

	// 新增对端只回传掩码（无可还原的现值）⇒ 显式失败，不落库。
	err = svc.UpdateConfigJSON(ctx, []byte(`{"p2p":{"federation":{"delivery_peers":[
		{"url":"https://peer-new.example.com","token":"********","name":"NEW"}]}}}`))
	require.Error(t, err, "掩码无法匹配现值必须显式失败（否则口令被静默换成掩码字符串）")
	assert.Contains(t, err.Error(), "delivery_peers[0].token", "错误须点名字段")
}
