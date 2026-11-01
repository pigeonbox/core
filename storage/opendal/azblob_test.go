package opendal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 回归：SharedKey 签名的 StringToSign 组装（逐字符确定性）。
// 算法参考 https://learn.microsoft.com/rest/api/storageservices/authorization-for-the-azure-storage-services
func TestAzBlobSignatureStringToSign(t *testing.T) {
	d, err := newAzBlobDriver(map[string]string{
		"account":   "acct",
		"container": "c1",
		"key":       "a2V5", // base64("key")
	})
	if err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodPut, "https://acct.blob.core.windows.net/c1/uploads/dir-a/file1.txt", nil)
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	req.Header.Set("x-ms-date", "Mon, 04 Oct 2026 00:00:00 GMT")
	req.Header.Set("x-ms-version", "2021-08-06")
	d.sign(req, 11)

	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "SharedKey acct:") {
		t.Fatalf("Authorization 形态错误: %q", auth)
	}
	sig := strings.TrimPrefix(auth, "SharedKey acct:")
	if len(sig) != 44 || !strings.Contains(sig, "=") {
		t.Fatalf("签名长度/形态异常: %q (len=%d)", sig, len(sig))
	}

	// 同一请求重复签名必须一致（确定性）；内容长度参与签名（改长度签名必须变）
	req2, _ := http.NewRequest(http.MethodPut, req.URL.String(), nil)
	req2.Header = req.Header.Clone()
	d.sign(req2, 11)
	if req2.Header.Get("Authorization") != auth {
		t.Fatal("同参数签名不一致")
	}
	req3, _ := http.NewRequest(http.MethodPut, req.URL.String(), nil)
	req3.Header = req.Header.Clone()
	d.sign(req3, 12)
	if req3.Header.Get("Authorization") == auth {
		t.Fatal("Content-Length 应参与签名")
	}

	// SAS 模式不生成 Authorization 头
	d2, err := newAzBlobDriver(map[string]string{
		"account": "acct", "container": "c1", "sas": "?sig=abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	req4, _ := http.NewRequest(http.MethodPut, d2.blobURL("a.txt"), nil)
	d2.sign(req4, 0)
	if req4.Header.Get("Authorization") != "" {
		t.Fatal("SAS 模式不应有 Authorization 头")
	}
	if !strings.Contains(req4.URL.RawQuery, "sig=abc") {
		t.Fatal("SAS token 应拼在 URL 上")
	}
}

// 配置校验：缺容器/缺凭据必须报错（不静默回退）
func TestAzBlobDriverValidation(t *testing.T) {
	if _, err := newAzBlobDriver(map[string]string{"account": "a"}); err == nil {
		t.Fatal("缺 container 应报错")
	}
	if _, err := newAzBlobDriver(map[string]string{"account": "a", "container": "c"}); err == nil {
		t.Fatal("缺 key/sas 应报错")
	}
	if _, err := newAzBlobDriver(map[string]string{"account": "a", "container": "c", "sas": "?sig=1"}); err != nil {
		t.Fatalf("sas 模式应合法: %v", err)
	}
}

// 回归（2026-11-02 core#36 真机实测）：canonicalized resource 的查询参数
// 必须是规范的 **name:value** 形态（不是 URL 的 name=value）。
//
// 背景：原实现直接拼 URL 的 "name=value" ⇒ 任何**带查询参数**的请求
// （Probe 的 comp=list、listAll 的 restype/comp/prefix）签名与规范不符
// ⇒ 真机 403 AuthorizationFailure；而原单测只验「签名字符串长什么样」
// ⇒ 恒绿 ⇒ 缺陷零阻力溜过。
func TestCanonicalizedResourceQueryFormat(t *testing.T) {
	u, err := url.Parse("https://acct.blob.core.windows.net/c1?restype=container&comp=list&prefix=a/b")
	if err != nil {
		t.Fatal(err)
	}
	cr := canonicalizedResource(u, "acct")

	// 账号在 host-style 端点下出现一次
	if !strings.HasPrefix(cr, "/acct/c1") {
		t.Fatalf("CR 前缀不对: %q", cr)
	}
	// 查询参数按名排序，每行 name:value
	want := []string{"\ncomp:list", "\nprefix:a/b", "\nrestype:container"}
	for _, w := range want {
		if !strings.Contains(cr, w) {
			t.Fatalf("CR 缺 %q，实际: %q", w, cr)
		}
	}
	if strings.Contains(cr, "comp=list") || strings.Contains(cr, "restype=container") {
		t.Fatalf("CR 查询参数仍用 name=value 形态: %q", cr)
	}
}

// 回归（2026-11-02）：path-style 端点（Azurite/模拟器）下账号出现**两次**
// ——这是 Azure 规范明确认可的形态（"the account name will appear twice ... expected"）。
func TestCanonicalizedResourcePathStyleAccountTwice(t *testing.T) {
	u, err := url.Parse("http://127.0.0.1:10000/devstoreaccount1/c1/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	cr := canonicalizedResource(u, "devstoreaccount1")
	if !strings.HasPrefix(cr, "/devstoreaccount1/devstoreaccount1/c1/f.txt") {
		t.Fatalf("path-style 下账号应出现两次: %q", cr)
	}
}

// 回归（2026-11-02 core#36 真机实测）：SharedKey 模式下容器级 URL 必须带 "?"。
// 原实现只在「有 SAS」时拼 "?" ⇒ 无 SAS 时查询串被直接黏在路径后面
// ⇒ 服务端 400 InvalidResourceName ⇒ Probe/List/RemoveAll 全线不可用。
func TestAzBlobContainerURLHasQuerySeparator(t *testing.T) {
	d, err := newAzBlobDriver(map[string]string{
		"account":   "acct",
		"container": "c1",
		"key":       "a2V5",
		"endpoint":  "https://acct.blob.core.windows.net",
	})
	if err != nil {
		t.Fatal(err)
	}
	u := d.containerURL("restype=container&comp=list")
	if !strings.Contains(u, "?") {
		t.Fatalf("containerURL 缺 '?' 分隔符: %q", u)
	}
	if strings.Contains(u, "c1restype") {
		t.Fatalf("containerURL 查询串被黏在路径后: %q", u)
	}
	// SAS 模式仍应正常
	d2, err := newAzBlobDriver(map[string]string{
		"account": "acct", "container": "c1", "sas": "sig=abc",
		"endpoint": "https://acct.blob.core.windows.net",
	})
	if err != nil {
		t.Fatal(err)
	}
	u2 := d2.containerURL("restype=container&comp=list")
	if !strings.Contains(u2, "?") || !strings.Contains(u2, "sig=abc") {
		t.Fatalf("SAS 模式 containerURL 不对: %q", u2)
	}
}

// 回归（2026-11-02 core#36 真机实测）：listAll 必须查 HTTP 状态码。
// 错误响应体也是合法 XML（`<Error><Code>…`），能被 Decoder「成功」解析成空 Blobs
// ⇒ 原实现把 403/404 当成「空列表」⇒ **鉴权失败伪装成「没有对象」**。
func TestAzBlobListAllRejectsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?><Error><Code>AuthorizationFailure</Code></Error>`))
	}))
	defer srv.Close()

	d, err := newAzBlobDriver(map[string]string{
		"account":   "acct",
		"container": "c1",
		"key":       "a2V5",
		"endpoint":  srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.listAll(context.Background(), "pre/")
	if err == nil {
		t.Fatal("listAll 对 403 必须报错（不得静默返回空列表）")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("错误信息应带状态码: %v", err)
	}
}
