//go:build live

// 存储驱动真机集成测试（**真服务**，非 mock）：Azure Blob + HDFS。
//
// 与 live_drivers_test.go 的 FTP/SFTP 用例同族，但这两个后端是
// 2026-11-02 core#36 的验收目标（此前只有单测/mock，无真机联调）。
//
// 为什么必须有这一层：
//   - Azure Blob 驱动是**手写 REST + 手写 SharedKey 签名**，单测只验「签名字符串
//     长什么样」，验不出「服务端认不认这个签名」⇒ 2026-11-02 真机实测查出**三个**
//     真实缺陷（见下），单测全绿而真机全线不可用。
//   - HDFS 驱动的手写 WebHDFS 客户端同样只验过 mock 响应。
//
// 前置（本机 / CI 均可）：
//
//	· Azurite（微软官方 Azure Blob 模拟器，走**真** Azure Blob REST 协议）：
//	    npx azurite --silent --location /tmp/azurite --blobPort 10000 \
//	        --queuePort 10001 --tablePort 10002
//	· HDFS（真 WebHDFS）：
//	    见 scripts/live-storage-services.sh（起 apache/hadoop 单节点 + WebHDFS）
//
// 环境变量（缺省即上面的本机默认端口）：
//
//	PB_LIVE_AZURE_ENDPOINT   默认 http://127.0.0.1:10000/devstoreaccount1
//	PB_LIVE_AZURE_ACCOUNT   默认 devstoreaccount1
//	PB_LIVE_AZURE_KEY       默认 Azurite 官方开发密钥
//	PB_LIVE_AZURE_CONTAINER 默认 pb-live-test
//	PB_LIVE_HDFS_ENDPOINT   默认 http://127.0.0.1:9870
//	PB_LIVE_HDFS_USER       默认 root
//
// 跑法：
//
//	go test -tags live ./storage/opendal/ -run Live -v -count=1
//
// ⚠️ 真机不可达时**跳过并显式报 SKIP**（退出码 0），不伪装成通过：
// 本地无 Docker/Azurite 是常态 ⇒ 硬失败会让开发者无法跑普通单测。
// 但 CI 门禁（scripts/check-live-storage-gate.sh）**要求真机可达**，
// 不可达即红（见该脚本注释）。
package opendal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	liveAzureAccountDefault = "devstoreaccount1"
	liveAzureKeyDefault     = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	liveContainerDefault    = "pb-live-test"
)

func liveEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// liveEndpointReachable 真机可达性探针（容器级 GET，认 HTTP 状态码即可）。
func liveEndpointReachable(url string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode > 0
}

// TestLiveAzureBlob 对**真** Azure Blob REST（Azurite）跑通驱动全链路。
//
// 覆盖 2026-11-02 真机实测修掉的三个缺陷（回归护栏）：
//  1. SharedKey 模式下 containerURL 未补 "?" ⇒ Probe/List/RemoveAll 全线 400；
//  2. canonicalized resource 的查询参数用了 URL 的 name=value 而非规范的 name:value
//     ⇒ 含查询参数的请求签名不匹配 ⇒ 403 AuthorizationFailure；
//  3. listAll 不查 HTTP 状态码 ⇒ 鉴权失败被当成「空列表」静默吞掉。
func TestLiveAzureBlob(t *testing.T) {
	endpoint := liveEnv("PB_LIVE_AZURE_ENDPOINT", "http://127.0.0.1:10000/"+liveAzureAccountDefault)
	account := liveEnv("PB_LIVE_AZURE_ACCOUNT", liveAzureAccountDefault)
	key := liveEnv("PB_LIVE_AZURE_KEY", liveAzureKeyDefault)
	container := liveEnv("PB_LIVE_AZURE_CONTAINER", liveContainerDefault)

	if !liveEndpointReachable(endpoint + "/" + container + "?restype=container&comp=list") {
		t.Skipf("SKIP: 真 Azure Blob（Azurite）不可达：%s —— 起 azurite 后重跑", endpoint)
	}

	ctx := context.Background()
	d, err := newAzBlobDriver(map[string]string{
		"account":   account,
		"container": container,
		"key":       key,
		"endpoint":  endpoint,
		"root":      "pb-live",
	})
	if err != nil {
		t.Fatalf("newAzBlobDriver: %v", err)
	}
	op, err := New(Config{
		Scheme:  SchemeAzBlob,
		Options: map[string]string{"account": account, "container": container, "key": key, "endpoint": endpoint, "root": "pb-live"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = d

	// 容器必须先存在（Azurite 不自动建容器）——用驱动的签名建。
	ensureLiveContainer(t, ctx, endpoint, account, key, container)

	// ── 缺陷 1 的回归护栏：容器级 URL 必须带 "?"（否则服务端 400 InvalidResourceName）
	cu := d.containerURL("restype=container&comp=list")
	if !strings.Contains(cu, "?") {
		t.Fatalf("containerURL 缺 '?' 分隔符：%q", cu)
	}
	if strings.Contains(cu, "pb-live-testrestype") {
		t.Fatalf("containerURL 查询串被黏在路径后：%q", cu)
	}

	// ── 缺陷 2 的回归护栏：canonicalized resource 的查询参数必须是 name:value
	cr := canonicalizedResource(mustURL(t, endpoint+"/"+container+"?restype=container&comp=list"), account)
	if !strings.Contains(cr, "\ncomp:list") {
		t.Fatalf("canonicalized resource 查询参数未用 name:value 形态：%q", cr)
	}
	if strings.Contains(cr, "comp=list") {
		t.Fatalf("canonicalized resource 查询参数仍用 name=value 形态：%q", cr)
	}

	// ── 全链路（真机）
	if err := Probe(ctx, op); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	body := []byte("live-azure-blob-check-9527")
	if err := op.Write(ctx, "dir-a/file1.txt", body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := op.Read(ctx, "dir-a/file1.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("Read 内容不符：%q", string(got))
	}
	md, err := op.Stat(ctx, "dir-a/file1.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if md.Size != int64(len(body)) {
		t.Fatalf("Stat size=%d want=%d", md.Size, len(body))
	}
	// 不存在 ⇒ os.ErrNotExist 契约（errors.Is 判定；os.IsNotExist 不解包 %w 链）
	if _, err := op.Stat(ctx, "dir-a/missing.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat 缺失文件应包装 os.ErrNotExist，实际=%v", err)
	}
	// 不存在的前缀 ⇒ 空列表、无错（与 S3 语义一致）
	ents, err := op.List(ctx, "no-such-prefix")
	if err != nil {
		t.Fatalf("List 缺失前缀应返回空列表无错，实际 err=%v", err)
	}
	if len(ents) != 0 {
		t.Fatalf("List 缺失前缀应为空，实际 %d 条", len(ents))
	}
	// ── 缺陷 3 的回归护栏：鉴权/容器错误不得伪装成「空列表」
	//     用一个**不存在的容器**触发 404 ⇒ listAll 必须报错。
	//     （此前 listAll 不查状态码 ⇒ 404 的 XML 错误体被 Decoder「成功」解析成
	//      空 Blobs ⇒ 返回空列表、无错误 ⇒ 调用方以为「桶里没东西」。）
	missing, err := newAzBlobDriver(map[string]string{
		"account":   account,
		"container": "pb-live-no-such-container",
		"key":       key,
		"endpoint":  endpoint,
		"root":      "pb-live",
	})
	if err != nil {
		t.Fatalf("newAzBlobDriver: %v", err)
	}
	if _, err := missing.listAll(ctx, "pb-live/"); err == nil {
		t.Fatal("listAll 对不存在的容器应报错（不得静默返回空列表）")
	}
	// 列举真实对象
	ents, err = op.List(ctx, "dir-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ents) == 0 {
		t.Fatal("List 未列出刚写入的对象")
	}
	// 清理
	if err := op.RemoveAll(ctx, "dir-a"); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if op.Exists(ctx, "dir-a/file1.txt") {
		t.Fatal("RemoveAll 后仍存在")
	}
}

// ensureLiveContainer 用驱动的 SharedKey 签名建容器（幂等）。
func ensureLiveContainer(t *testing.T, ctx context.Context, endpoint, account, key, container string) {
	t.Helper()
	d, err := newAzBlobDriver(map[string]string{
		"account": account, "container": container, "key": key, "endpoint": endpoint,
	})
	if err != nil {
		t.Fatalf("newAzBlobDriver: %v", err)
	}
	url := fmt.Sprintf("%s/%s?restype=container", endpoint, container)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, nil)
	if err != nil {
		t.Fatalf("建容器请求: %v", err)
	}
	d.sign(req, 0)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("建容器: %v", err)
	}
	_ = resp.Body.Close()
	// 201=新建 409=已存在 ⇒ 两者都算成功
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		t.Fatalf("建容器失败：HTTP %d", resp.StatusCode)
	}
}

// mustURL 解析 URL（测试辅助）。
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("URL 解析失败 %q: %v", raw, err)
	}
	return u
}
