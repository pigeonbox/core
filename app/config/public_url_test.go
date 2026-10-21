// public_url_test.go —— base.public_url（站点公网地址）校验与生效口径。
//
// 这个字段 2026-10-22 落地：要解决的问题是「分享链接原先按请求 origin 拼，
// NAS/内网部署时生成的链接带内网 IP，外部用户打不开」。
//
// 口径要点（与 p2p.federation.public_url 刻意不同）：
//   - 它只被**展示与复制**，服务端从不取它 ⇒ 私网地址（192.168.x / 10.x）
//     是合法输入（局域网内的 NAS 就是这个形态），不套 SSRF 私网策略；
//   - 但 javascript:/data: 这类可执行 scheme 必须拒（会被前端拼进链接）。
package config

import (
	"strings"
	"testing"
)

// TestValidatePublicSiteURL 站点公网地址的形态边界。
func TestValidatePublicSiteURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "https 公网域名", raw: "https://box.example.com"},
		{name: "http 公网域名", raw: "http://box.example.com"},
		{name: "带路径", raw: "https://box.example.com/pb"},
		{name: "带端口", raw: "https://box.example.com:8443"},
		// 局域网 NAS 是正常用法，不能被私网策略拦（这是与
		// security.ValidateEndpointURL 的关键差异）。
		{name: "局域网地址", raw: "http://192.168.1.10:12345"},
		{name: "局域网地址无端口", raw: "http://10.0.0.5"},
		{name: "javascript scheme", raw: "javascript:alert(1)", wantErr: true},
		{name: "data scheme", raw: "data:text/html,<script>", wantErr: true},
		{name: "file scheme", raw: "file:///etc/passwd", wantErr: true},
		{name: "缺 scheme", raw: "box.example.com", wantErr: true},
		{name: "缺 host", raw: "https://", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validatePublicSiteURL(c.raw)
			if c.wantErr && err == nil {
				t.Fatalf("期望报错，实际通过：%q", c.raw)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("期望通过，实际报错：%q → %v", c.raw, err)
			}
		})
	}
}

// TestValidateSystemConfigBasePublicURL 空值放行、非法值拦下。
func TestValidateSystemConfigBasePublicURL(t *testing.T) {
	// 空 = 未配置 = 放行（前端回退 location.origin）
	if err := validateSystemConfig(&SystemConfig{Base: &BaseConfig{Name: "n"}}); err != nil {
		t.Fatalf("未配置 public_url 应放行，实际报错：%v", err)
	}
	// 纯空白也当未配置
	if err := validateSystemConfig(&SystemConfig{Base: &BaseConfig{PublicURL: "   "}}); err != nil {
		t.Fatalf("空白 public_url 应放行，实际报错：%v", err)
	}
	// 合法值放行
	if err := validateSystemConfig(&SystemConfig{Base: &BaseConfig{PublicURL: "https://box.example.com"}}); err != nil {
		t.Fatalf("合法 public_url 应放行，实际报错：%v", err)
	}
	// 非法 scheme 拦下，且错误信息要指出字段名
	err := validateSystemConfig(&SystemConfig{Base: &BaseConfig{PublicURL: "javascript:alert(1)"}})
	if err == nil {
		t.Fatal("javascript: scheme 应被拒")
	}
	if !strings.Contains(err.Error(), "base.public_url") {
		t.Fatalf("错误信息应含字段名 base.public_url，实际：%v", err)
	}
}
