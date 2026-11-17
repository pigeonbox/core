package opendal

// recover_test.go —— 远端后端「先 200 → 后 503 / 直接关停 → 恢复」的
// **故障注入**（L6 故障注入）。
//
// 注入什么故障：
//   用 httptest 起一个**真 HTTP** 的假对象后端，三种可切换形态：
//     · ok          —— 200 + 对象内容（后端正常）；
//     · unavailable —— 503 Service Unavailable（后端过载/维护）；
//     · dead        —— 接受连接后直接掐断（后端进程被杀/网络断）。
//   驱动层用真 HTTP 请求打过去（不走内存桩）⇒ 状态码与连接错误都是真的。
//
// 断言什么（全部是**行为**）：
//   ① 后端 200 ⇒ 读得到内容、Stat 得到真实大小（基线）。
//   ② 后端 503 ⇒ 读**必须失败**，且错误**不得伪装成「对象不存在」**
//      （os.ErrNotExist）——伪装的后果是取件面报「分享没了」、备份面把对象
//      记成 missing 悄悄漏掉；错误里必须看得到「503」这个真实原因。
//   ③ 后端直接关停（连接被掐断）⇒ 读失败，且**同样不得伪装成「不存在」**
//      （连接错误 ≠ 404）。
//   ④ 后端恢复 ⇒ **同一个对象**照常读得到（客户端没被故障毒化：
//      不粘死在失败态、不缓存失败结果）。
//
// 怎么让它变红（负对照，详见交付说明的实跑记录）：
//   - 把驱动里「非 200 一律按 os.ErrNotExist 返回」⇒ ②③红
//     （后端故障被装成「对象不存在」）；
//   - 把 Operator/驱动改成「失败后粘住（记住首次失败）」⇒ ④红
//     （恢复后仍读不到）。
//
// 注意：本文件只到 Operator 层（storage 包那侧「取件不伪装 404」的判据见
// storage/fault_driver_test.go 的 TestStorage_BackendDown_DoesNotDisguiseAsNotFound）。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 后端形态。
const (
	backendOK          = "ok"
	backendUnavailable = "unavailable"
	backendDead        = "dead"
)

// httpFaultBackend 可切换形态的假对象后端（真 HTTP）。
type httpFaultBackend struct {
	mu      sync.Mutex
	mode    string
	content []byte
}

func (b *httpFaultBackend) setMode(mode string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mode = mode
}

func (b *httpFaultBackend) current() (string, []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mode, append([]byte(nil), b.content...)
}

// httpFaultDriver 打真 HTTP 的假驱动（测试专用）。
type httpFaultDriver struct {
	base string
}

func (d *httpFaultDriver) url(key string) string { return d.base + "/" + key }

// do 发一个请求，返回响应体与状态码。
func (d *httpFaultDriver) do(ctx context.Context, method, key string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, d.url(key), nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// 连接层失败（后端关停/网络断）⇒ 原样上抛，**不得**换成 ErrNotExist。
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return nil, resp.StatusCode, rerr
	}
	return body, resp.StatusCode, nil
}

func (d *httpFaultDriver) Write(ctx context.Context, key string, data []byte) error {
	_, code, err := d.do(ctx, http.MethodPut, key)
	if err != nil {
		return err
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return fmt.Errorf("write %s: unexpected status %d", key, code)
	}
	return nil
}

func (d *httpFaultDriver) WriteStream(ctx context.Context, key string, r io.Reader, size int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return d.Write(ctx, key, data)
}

func (d *httpFaultDriver) Read(ctx context.Context, key string) ([]byte, error) {
	body, code, err := d.do(ctx, http.MethodGet, key)
	if err != nil {
		return nil, err // 连接错误 ⇒ 原样上抛
	}
	switch code {
	case http.StatusOK:
		return body, nil
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", os.ErrNotExist, key)
	default:
		// 后端故障（5xx 等）⇒ 错误里带真实状态码，**不得**退化成 ErrNotExist。
		return nil, fmt.Errorf("read %s: backend returned %d %s", key, code, http.StatusText(code))
	}
}

func (d *httpFaultDriver) Reader(ctx context.Context, key string) (io.ReadCloser, error) {
	body, err := d.Read(ctx, key)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(string(body))), nil
}

func (d *httpFaultDriver) Stat(ctx context.Context, key string) (*Metadata, error) {
	body, code, err := d.do(ctx, http.MethodHead, key)
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusOK:
		return &Metadata{Path: key, Size: int64(len(body))}, nil
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", os.ErrNotExist, key)
	default:
		return nil, fmt.Errorf("stat %s: backend returned %d %s", key, code, http.StatusText(code))
	}
}

func (d *httpFaultDriver) Delete(ctx context.Context, key string) error {
	_, code, err := d.do(ctx, http.MethodDelete, key)
	if err != nil {
		return err
	}
	if code != http.StatusOK && code != http.StatusNoContent {
		return fmt.Errorf("delete %s: unexpected status %d", key, code)
	}
	return nil
}

func (d *httpFaultDriver) RemoveAll(ctx context.Context, key string) error {
	_, code, err := d.do(ctx, http.MethodDelete, key)
	if err != nil {
		return err
	}
	if code != http.StatusOK && code != http.StatusNoContent {
		return fmt.Errorf("remove %s: unexpected status %d", key, code)
	}
	return nil
}

// newFaultBackendServer 起一个可切换形态的假对象后端 + 驱动 + Operator。
func newFaultBackendServer(t *testing.T) (*httpFaultBackend, *Operator) {
	t.Helper()
	backend := &httpFaultBackend{mode: backendOK, content: []byte("payload-bytes")}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode, content := backend.current()
		switch mode {
		case backendUnavailable:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("Service Unavailable"))
		case backendNotFound:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("NoSuchKey"))
		case backendDead:
			// 后端「进程被杀」：接受连接后直接掐断（不发任何响应）。
			hj, ok := w.(http.Hijacker)
			if !ok {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(content)
		}
	}))
	t.Cleanup(srv.Close)

	return backend, NewCustom(SchemeS3, &httpFaultDriver{base: srv.URL})
}

// TestOperator_RemoteBackend503ThenRecovery 远端后端 503 ⇒ 语义明确 +
// 恢复后同一对象可取。
func TestOperator_RemoteBackend503ThenRecovery(t *testing.T) {
	backend, op := newFaultBackendServer(t)
	ctx := context.Background()
	const key = "uploads/2026/11/18/remote.bin"

	// ① 基线：后端正常 ⇒ 读得到内容。
	data, err := op.Read(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, "payload-bytes", string(data))

	// ② 后端 503 ⇒ 读失败，且不得伪装成「对象不存在」。
	backend.setMode(backendUnavailable)
	_, err = op.Read(ctx, key)
	require.Error(t, err, "后端 503 时读必须失败")
	assert.NotErrorIs(t, err, os.ErrNotExist,
		"后端故障不得伪装成「对象不存在」（取件会误报分享没了、备份会漏对象）")
	assert.Contains(t, err.Error(), "503", "错误里必须看得到真实状态码（语义明确）")

	// Stat 同样不得伪装。
	_, err = op.Stat(ctx, key)
	require.Error(t, err)
	assert.NotErrorIs(t, err, os.ErrNotExist, "Stat 同样不得伪装成「对象不存在」")

	// ④ 后端恢复 ⇒ 同一个对象照常读得到（没粘死在失败态）。
	backend.setMode(backendOK)
	data, err = op.Read(ctx, key)
	require.NoError(t, err, "后端恢复后同一对象必须照常读得到")
	assert.Equal(t, "payload-bytes", string(data))
}

// TestOperator_RemoteBackendDeadThenRecovery 远端后端**直接关停**
// （连接被掐断）⇒ 读失败且不得伪装成「对象不存在」；恢复后可取。
func TestOperator_RemoteBackendDeadThenRecovery(t *testing.T) {
	backend, op := newFaultBackendServer(t)
	ctx := context.Background()
	const key = "uploads/2026/11/18/dead.bin"

	data, err := op.Read(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, "payload-bytes", string(data))

	// 后端直接关停。
	backend.setMode(backendDead)
	_, err = op.Read(ctx, key)
	require.Error(t, err, "后端关停时读必须失败")
	assert.NotErrorIs(t, err, os.ErrNotExist,
		"连接错误不得伪装成「对象不存在」（取件会误报分享没了）")

	// 恢复 ⇒ 同一个对象照常可取。
	backend.setMode(backendOK)
	data, err = op.Read(ctx, key)
	require.NoError(t, err, "后端恢复后同一对象必须照常读得到")
	assert.Equal(t, "payload-bytes", string(data))
}

// TestOperator_RemoteBackend404IsStillNotFound 对照组：真正的 404 **仍要**
// 判成「对象不存在」（否则「后端故障不伪装」这条判据会退化成「一切都不算
// 不存在」——那会让真缺失的对象被当成故障反复重试）。
func TestOperator_RemoteBackend404IsStillNotFound(t *testing.T) {
	backend, op := newFaultBackendServer(t)
	ctx := context.Background()
	const key = "uploads/2026/11/18/missing.bin"

	// 让后端对这一路径回 404（形态 ok 但路径命中缺失分支）。
	backend.setMode(backendNotFound)
	_, err := op.Read(ctx, key)
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist, "真 404 必须仍判成「对象不存在」")
}

// backendNotFound 形态：404。
const backendNotFound = "notfound"
