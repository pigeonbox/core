// webdav_integration_test.go —— L3 集成层：WebDAV 后端**真服务**集成用例。
//
// 为什么要有这一层（此前 WebDAV 只有单测级覆盖）：
//   - storage/opendal/opendal_test.go 的 httptestWebDAV 只验「驱动自身」；
//   - **产品真正调用的是 storage.StorageService 的对外 API**
//     （SaveFile / SaveBytes / GetFile / GetFileReader / FileExists /
//     GetFileSize / StatFile / DeleteFile / SaveChunk / MergeChunks /
//     CleanChunks / ListRemoteObjects / ProbeConfig），这一层此前**零真后端覆盖**；
//   - 分片合并（chunk 语义）在真 WebDAV 上的行为（顺序、合并后清理、缺片报错）
//     与 fake 驱动不一致 ⇒ 「单测全绿」≠「真机可用」。
//
// 本文件用 golang.org/x/net/webdav 在**进程内**起真 WebDAV 服务
// （仿 storage/opendal/opendal_test.go 的 httptestWebDAV），
// 因此可以在普通 `go test ./...` 里**真跑**：不需要容器、不需要外部服务。
//
// 负对照（fault injection）：
//   - PB_TEST_WEBDAV_FAULT=write ⇒ 写入路径被注入 500 ⇒ 生命周期里的
//     「写入应成功」断言必须**变红**（证明断言不是空转）；
//   - PB_TEST_WEBDAV_FAULT=read  ⇒ 读取路径被注入 500 ⇒ 同理。
//     复原（不设该 env）后复跑必须转绿。
package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/webdav"
)

// webdavFaultInjector 在真 WebDAV 服务前面挂一层故障注入：
// 命中「方法 + 路径后缀」时直接返回 5xx（模拟服务端故障），并记录命中次数。
//
// 用它做两件事：
//  1. 负对照：注入故障 ⇒ 断言必须变红（证明断言真的打到了后端）；
//  2. 错误语义：注入故障 ⇒ 对外 API 必须**显式报错**（不许静默成功/返回空）。
type webdavFaultInjector struct {
	inner http.Handler

	mu           sync.Mutex
	armed        bool   // 是否已启用注入（默认关闭：关闭时所有请求原样透传）
	method       string // 命中方法（空 = 任意）
	pathContains string // 命中路径包含片段（空 = 任意）
	status       int    // 注入的状态码
	hits         int
}

// ServeHTTP 命中注入条件 ⇒ 5xx；否则透传给真 WebDAV handler。
func (f *webdavFaultInjector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	// ⚠️ 未启用注入时必须**原样透传**（armed 单独判：否则「空条件」会被当成
	// 「匹配全部」，把整个服务打成 500 —— 那是测试自身的 bug，不是后端行为）。
	matched := f.armed &&
		(f.method == "" || f.method == r.Method) &&
		(f.pathContains == "" || strings.Contains(r.URL.Path, f.pathContains))
	if matched {
		f.hits++
		status := f.status
		f.mu.Unlock()
		if status == 0 {
			status = http.StatusInternalServerError
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte("injected webdav fault"))
		return
	}
	f.mu.Unlock()
	f.inner.ServeHTTP(w, r)
}

// Arm 开故障注入（method/pathContains 任一为空 = 匹配任意）。
func (f *webdavFaultInjector) Arm(method, pathContains string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed, f.method, f.pathContains, f.status = true, method, pathContains, status
}

// Disarm 关故障注入。
func (f *webdavFaultInjector) Disarm() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed, f.method, f.pathContains, f.status = false, "", "", 0
}

// Hits 注入命中次数（用来证明「故障确实注入了」，避免假绿）。
func (f *webdavFaultInjector) Hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// newWebDAVIntegrationServer 在进程内临时目录起真 WebDAV 服务。
// 返回 server、server 根目录（可直读文件，验证对象真落在后端磁盘上）、故障注入器。
func newWebDAVIntegrationServer(t *testing.T) (*httptest.Server, string, *webdavFaultInjector) {
	t.Helper()
	root := t.TempDir()
	inner := &webdav.Handler{
		FileSystem: webdav.Dir(root),
		LockSystem: webdav.NewMemLS(),
	}
	fault := &webdavFaultInjector{inner: inner}
	srv := httptest.NewServer(fault)
	t.Cleanup(srv.Close)
	return srv, root, fault
}

// newWebDAVIntegrationService 用真 WebDAV 后端构造 StorageService。
//
// 用 NewStorageServiceE（不是兼容入口 NewStorageService）：后者在构造失败时
// 会**静默降级 local**——那样测试会在本地文件系统上「全绿」而根本没碰后端。
func newWebDAVIntegrationService(t *testing.T, url string) *StorageService {
	t.Helper()
	svc, err := NewStorageServiceE(&StorageConfig{
		Type:      StorageTypeWebDAV,
		DataPath:  t.TempDir(),
		BaseURL:   "http://webdav.test",
		WebDAVURL: url,
		Root:      "pigeonbox",
	})
	require.NoError(t, err, "WebDAV 后端构造失败")
	require.Equal(t, StorageTypeWebDAV, svc.EffectiveType(), "后端被降级成 local（测试会失去意义）")
	require.NoError(t, svc.InitError())
	return svc
}

// TestWebDAVIntegrationLifecycle 真 WebDAV 上的完整生命周期（写→读→覆盖→
// 存在→大小→删→再读→分片合并→列表）。
//
// 负对照开关 PB_TEST_WEBDAV_FAULT=write|read：注入真服务故障后，
// 本用例的「写入/读取应成功」断言必须变红（否则断言是空转的）。
func TestWebDAVIntegrationLifecycle(t *testing.T) {
	srv, root, fault := newWebDAVIntegrationServer(t)
	svc := newWebDAVIntegrationService(t, srv.URL)
	const prefix = "uploads/2026/11/18"

	// 负对照：注入真服务故障（默认不注入）
	switch mode := strings.TrimSpace(os.Getenv("PB_TEST_WEBDAV_FAULT")); mode {
	case "":
	case "write":
		fault.Arm(http.MethodPut, "/"+prefix+"/", http.StatusInternalServerError)
	case "read":
		fault.Arm(http.MethodGet, "/"+prefix+"/", http.StatusInternalServerError)
	default:
		t.Fatalf("未知的 PB_TEST_WEBDAV_FAULT=%q（只支持 write / read / 空）", mode)
	}

	exerciseRemoteStorageLifecycle(t, svc, prefix)

	// 负对照跑到这里说明注入没生效（断言全绿）⇒ 明确报错，避免「以为注入了其实没注入」
	if mode := strings.TrimSpace(os.Getenv("PB_TEST_WEBDAV_FAULT")); mode != "" {
		t.Fatalf("PB_TEST_WEBDAV_FAULT=%s 已注入故障，但生命周期断言仍全绿（断言没打到后端？）", mode)
	}

	// 对象真落在 WebDAV 服务端的磁盘上（不是只在客户端内存里转了一圈）：
	// 且**必须落在配置的 root 子目录内**（不许写到 root 之外）。
	onDisk, err := os.ReadFile(filepath.Join(root, "pigeonbox", filepath.FromSlash(prefix), "multipart.bin"))
	require.NoError(t, err, "对象应真实落在 WebDAV 服务端磁盘上")
	require.Equal(t, "multipart payload", string(onDisk))
	_, err = os.Stat(filepath.Join(root, filepath.FromSlash(prefix), "multipart.bin"))
	require.Error(t, err, "对象不许写到配置的 root 之外")
}

// TestWebDAVIntegrationChunkMerge 分片语义专项：合并顺序、缺片报错、显式清理。
func TestWebDAVIntegrationChunkMerge(t *testing.T) {
	srv, _, _ := newWebDAVIntegrationServer(t)
	svc := newWebDAVIntegrationService(t, srv.URL)
	ctx := context.Background()

	// 合并顺序：chunk_0 → chunk_1 → chunk_2
	parts := [][]byte{[]byte("part-0|"), []byte("part-1|"), []byte("part-2|")}
	for i, p := range parts {
		require.NoError(t, svc.SaveChunk(ctx, "order-1", i, p))
	}
	require.NoError(t, svc.MergeChunks(ctx, "order-1", 3, "uploads/merged-order.bin"))
	got, err := svc.GetFile(ctx, "uploads/merged-order.bin")
	require.NoError(t, err)
	require.Equal(t, "part-0|part-1|part-2|", string(got), "合并产物必须按分片序号拼接")

	// 缺片 ⇒ 合并必须报错，且**不许产出半截文件**
	require.NoError(t, svc.SaveChunk(ctx, "missing-1", 0, []byte("only-first")))
	err = svc.MergeChunks(ctx, "missing-1", 2, "uploads/merged-missing.bin")
	require.Error(t, err, "缺片时合并必须报错")
	require.False(t, svc.FileExists(ctx, "uploads/merged-missing.bin"), "合并失败不许留下半截产物")

	// 显式清理：分片目录被删干净
	require.NoError(t, svc.CleanChunks(ctx, "order-1"))
	require.False(t, svc.FileExists(ctx, "chunks/order-1/chunk_0"))
	require.False(t, svc.FileExists(ctx, "chunks/order-1/chunk_1"))
	require.False(t, svc.FileExists(ctx, "chunks/order-1/chunk_2"))
}

// TestWebDAVIntegrationFaultSurfacing 故障注入下的**错误语义**：
// 对外 API 必须显式报错，不许静默成功、不许返回空内容冒充成功。
func TestWebDAVIntegrationFaultSurfacing(t *testing.T) {
	srv, _, fault := newWebDAVIntegrationServer(t)
	svc := newWebDAVIntegrationService(t, srv.URL)
	ctx := context.Background()

	// 写入故障 ⇒ SaveBytes 必须报错，且对象不许出现
	fault.Arm(http.MethodPut, "/faults/write.txt", http.StatusInternalServerError)
	err := svc.SaveBytes(ctx, "faults/write.txt", []byte("payload"))
	require.Error(t, err, "写入失败必须显式报错")
	require.Positive(t, fault.Hits(), "故障注入必须真的命中（否则本用例是假绿）")
	require.False(t, svc.FileExists(ctx, "faults/write.txt"), "写入失败不许留下对象")

	// 读故障 ⇒ GetFile 必须报错（不许返回空内容）
	fault.Disarm()
	require.NoError(t, svc.SaveBytes(ctx, "faults/read.txt", []byte("payload")))
	fault.Arm(http.MethodGet, "/faults/read.txt", http.StatusInternalServerError)
	_, err = svc.GetFile(ctx, "faults/read.txt")
	require.Error(t, err, "读取失败必须显式报错")
	require.True(t, svc.FileExists(ctx, "faults/read.txt"), "对象仍在（存在性用 PROPFIND，不受 GET 故障影响）")

	// 流式读同样必须报错
	_, _, err = svc.GetFileReader(ctx, "faults/read.txt")
	require.Error(t, err, "流式读取失败必须显式报错")

	// 删除故障 ⇒ DeleteFile 必须报错
	fault.Disarm()
	require.NoError(t, svc.SaveBytes(ctx, "faults/delete.txt", []byte("payload")))
	fault.Arm(http.MethodDelete, "/faults/delete.txt", http.StatusInternalServerError)
	err = svc.DeleteFile(ctx, "faults/delete.txt")
	require.Error(t, err, "删除失败必须显式报错")
	require.True(t, svc.FileExists(ctx, "faults/delete.txt"), "删除失败后对象必须还在")
}

// TestWebDAVIntegrationProbe 认证级探测（管理端「测试连接」走的就是它）。
func TestWebDAVIntegrationProbe(t *testing.T) {
	srv, _, _ := newWebDAVIntegrationServer(t)
	cfg := &StorageConfig{
		Type:      StorageTypeWebDAV,
		DataPath:  t.TempDir(),
		BaseURL:   "http://webdav.test",
		WebDAVURL: srv.URL,
		Root:      "pigeonbox",
	}
	ctx := context.Background()

	// 根目录还不存在 ⇒ 探测必须报错（不许把「连不上」当「连上了」）
	require.Error(t, ProbeConfig(ctx, cfg), "根目录不存在时探测应失败")

	// 建出根目录后再探测 ⇒ 通过
	svc, err := NewStorageServiceE(cfg)
	require.NoError(t, err)
	require.NoError(t, svc.SaveBytes(ctx, "probe.txt", []byte("x")))
	require.NoError(t, ProbeConfig(ctx, cfg))
	require.NoError(t, BuildAndProbe(ctx, cfg))
}
