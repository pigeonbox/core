package federation

// 跨站投递（联邦前台化，2026-11-01）的域层断言。
//
// 覆盖四个口径（每个都有负对照）：
//  1. 对端 URL 归一化（访客多打一个斜杠/大小写不同也要能命中）；
//  2. 对端白名单命中（未登记 ⇒ 不命中 ⇒ 上层当场拒，**不发任何出站请求**）；
//  3. 共享口令常数时间比对（无口令/错口令 ⇒ 拒；口令即权限）；
//  4. 出站：铸槽位带口令、转发**流式**（不整文件读进内存 ⇒ 请求体 chunked）。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
)

// withDeliveryPeers 临时替换全局 conf 里的可投递对端（测试隔离用）。
func withDeliveryPeers(t *testing.T, peers ...conf.FederationDeliveryPeer) {
	t.Helper()
	old := conf.GetGlobalConfig()
	cfg := &conf.AppConfiguration{}
	if old != nil {
		*cfg = *old
	}
	cfg.Federation.DeliveryPeers = peers
	conf.SetGlobalConfig(cfg)
	t.Cleanup(func() { conf.SetGlobalConfig(old) })
}

// TestNormalizePeerURL 对端基址归一化。
func TestNormalizePeerURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "http://10.10.30.253:12345", want: "http://10.10.30.253:12345"},
		{in: "http://10.10.30.253:12345/", want: "http://10.10.30.253:12345"},
		{in: "HTTP://B.Example.com", want: "http://b.example.com"},
		{in: "https://b.example.com:443/", want: "https://b.example.com"},
		{in: "http://b.example.com:80", want: "http://b.example.com"},
		{in: "https://b.example.com/box/", want: "https://b.example.com/box"},
		{in: "https://b.example.com/?x=1", want: "https://b.example.com"},
		{in: "", wantErr: true},
		{in: "ftp://b.example.com", wantErr: true},
		{in: "http://", wantErr: true},
		{in: "http://user:pass@b.example.com", wantErr: true},
	}
	for _, c := range cases {
		got, err := NormalizePeerURL(c.in)
		if c.wantErr {
			assert.Error(t, err, "输入 %q 应当被拒", c.in)
			continue
		}
		require.NoError(t, err, "输入 %q", c.in)
		assert.Equal(t, c.want, got, "输入 %q", c.in)
	}
}

// TestMatchDeliveryPeer 对端白名单命中口径。
//
// 负对照：把 MatchDeliveryPeer 里的 `base == target` 改成 `strings.HasPrefix(base, target)`
// ⇒ 「未登记的 URL 也能命中」这条断言必红。
func TestMatchDeliveryPeer(t *testing.T) {
	withDeliveryPeers(t,
		conf.FederationDeliveryPeer{URL: "http://10.10.30.253:12345", Token: "shared-token"},
		conf.FederationDeliveryPeer{URL: "https://b.example.com", Token: "other-token"},
	)

	// 命中（访客多打一个尾斜杠也要命中）
	p, ok := MatchDeliveryPeer("http://10.10.30.253:12345/")
	require.True(t, ok, "已登记的对端必须命中")
	assert.Equal(t, "shared-token", p.Token)

	// 大小写归一后命中
	_, ok = MatchDeliveryPeer("HTTP://10.10.30.253:12345")
	assert.True(t, ok, "归一化后应命中")

	// 未登记 ⇒ 不命中（⇒ 上层当场拒，不发出站请求）
	_, ok = MatchDeliveryPeer("http://evil.example.com")
	assert.False(t, ok, "未登记的对端不得命中")

	// 非法 URL ⇒ 不命中
	_, ok = MatchDeliveryPeer("not-a-url")
	assert.False(t, ok, "非法地址不得命中")
}

// TestVerifyDeliveryToken 共享口令比对（口令即权限）。
//
// 负对照：把 subtle.ConstantTimeCompare 换成 `==` ⇒ 「错口令也通过」这条断言必红。
func TestVerifyDeliveryToken(t *testing.T) {
	withDeliveryPeers(t,
		conf.FederationDeliveryPeer{URL: "https://b.example.com", Token: "shared-token"},
		conf.FederationDeliveryPeer{URL: "https://c.example.com", Token: "other-token"},
	)

	assert.False(t, VerifyDeliveryToken(""), "无口令必须拒")
	assert.False(t, VerifyDeliveryToken("wrong-token"), "错口令必须拒")
	assert.False(t, VerifyDeliveryToken("shared-toke"), "前缀口令必须拒")
	assert.True(t, VerifyDeliveryToken("shared-token"), "任一登记对端的口令匹配即通过")
	assert.True(t, VerifyDeliveryToken("other-token"), "第二个对端的口令也应通过")
}

// TestVerifyDeliveryToken_NoPeers 没有任何登记对端 ⇒ 一律拒。
func TestVerifyDeliveryToken_NoPeers(t *testing.T) {
	withDeliveryPeers(t)
	assert.False(t, VerifyDeliveryToken("anything"))
}

// TestCreateSlot 铸槽位：带口令 bearer、只回 token。
func TestCreateSlot(t *testing.T) {
	var gotHeader string
	var gotForm map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get(DeliveryTokenHeader)
		require.NoError(t, r.ParseForm())
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"token":"tok-abc"}}`))
	}))
	defer srv.Close()

	c := NewDeliveryClient()
	tok, err := c.CreateSlot(context.Background(),
		conf.FederationDeliveryPeer{URL: srv.URL, Token: "shared-token"},
		"发起柜", "留言", "标题")
	require.NoError(t, err)
	assert.Equal(t, "tok-abc", tok)
	assert.Equal(t, "shared-token", gotHeader, "必须带共享口令 bearer")
	assert.Equal(t, []string{"发起柜"}, gotForm["from_name"])
	assert.Equal(t, []string{"留言"}, gotForm["note"])
	assert.Equal(t, []string{"标题"}, gotForm["title"])
}

// TestCreateSlot_PeerRejects 对端拒绝（业务码非 0 / 非 200）⇒ 报错，不静默。
func TestCreateSlot_PeerRejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":10003,"message":"共享口令无效或未登记"}`))
	}))
	defer srv.Close()

	c := NewDeliveryClient()
	_, err := c.CreateSlot(context.Background(),
		conf.FederationDeliveryPeer{URL: srv.URL, Token: "bad"}, "", "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "共享口令无效")
}

// TestForwardUpload_StreamsFile 转发：**流式**（请求体 chunked）+ 内容不丢。
//
// 负对照：把转发实现改成「先 io.ReadAll 再一次性写出」⇒ `r.ContentLength == -1`
// 这条断言必红（整文件缓冲 ⇒ 长度已知 ⇒ Content-Length 有值）。
func TestForwardUpload_StreamsFile(t *testing.T) {
	const payload = "hello-federation-delivery-payload"

	var gotContentLength int64
	var gotChunked bool
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentLength = r.ContentLength
		// ⚠️ net/http 服务端把 Transfer-Encoding 从 Header 里摘掉，放在
		// r.TransferEncoding（读 Header 永远是空 ⇒ 别读错地方）。
		for _, te := range r.TransferEncoding {
			if te == "chunked" {
				gotChunked = true
			}
		}
		require.NoError(t, r.ParseMultipartForm(1<<20))
		fhs := r.MultipartForm.File["files"]
		require.Len(t, fhs, 1)
		f, err := fhs[0].Open()
		require.NoError(t, err)
		defer func() { _ = f.Close() }()
		b, err := io.ReadAll(f)
		require.NoError(t, err)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"投递成功","data":{"file_count":1,"pickup_code":"AB12CD"}}`))
	}))
	defer srv.Close()

	c := NewDeliveryClient()
	res, err := c.ForwardUpload(context.Background(),
		conf.FederationDeliveryPeer{URL: srv.URL, Token: "shared-token"},
		"tok-abc",
		[]DeliveryFile{{
			Name: "a.txt",
			Size: int64(len(payload)),
			Open: func() (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(payload)), nil
			},
		}})
	require.NoError(t, err)
	assert.Equal(t, 1, res.FileCount)
	assert.Equal(t, "AB12CD", res.PickupCode, "必须透传对方柜铸的取件码")
	assert.Equal(t, payload, gotBody, "文件内容不得丢失")
	assert.Equal(t, int64(-1), gotContentLength, "转发必须是流式（长度未知 ⇒ chunked）")
	assert.True(t, gotChunked, "转发请求应是 chunked（未整文件缓冲）")
}

// TestForwardUpload_PropagatesPeerError 对端业务失败 ⇒ 报错（不吞）。
func TestForwardUpload_PropagatesPeerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":10001,"message":"投递链接不存在或已撤销"}`))
	}))
	defer srv.Close()

	c := NewDeliveryClient()
	_, err := c.ForwardUpload(context.Background(),
		conf.FederationDeliveryPeer{URL: srv.URL, Token: "t"}, "tok",
		[]DeliveryFile{{Name: "a.txt", Open: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("x")), nil
		}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "投递链接不存在")
}

// TestDeliveryClient_NoRedirectFollow 跟随重定向会把对端重定向目标变成
// 不受控的出站地址（SSRF 绕过）⇒ 3xx 必须当失败。
func TestDeliveryClient_NoRedirectFollow(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	c := NewDeliveryClient()
	_, err := c.CreateSlot(context.Background(),
		conf.FederationDeliveryPeer{URL: srv.URL, Token: "t"}, "", "", "")
	require.Error(t, err, "3xx 必须判失败（不跟随）")
	assert.Equal(t, 1, hits, "不得跟随重定向再打一次")
}

// TestForwardUpload_EmptyFiles 没有文件 ⇒ 直接失败（不发请求）。
func TestForwardUpload_EmptyFiles(t *testing.T) {
	c := NewDeliveryClient()
	_, err := c.ForwardUpload(context.Background(),
		conf.FederationDeliveryPeer{URL: "http://b.example.com", Token: "t"}, "tok", nil)
	require.Error(t, err)
}

// TestForwardUpload_TrulyLazy 流式的**行为**断言：文件内容必须「边读边发」。
//
// 文件读取器在第一次 Read 之前会阻塞，直到对端**开始读 body** 才放行：
//
//	· 流式实现（io.Pipe + io.Copy）：请求头先到 ⇒ 对端 handler 已起 ⇒ 放行；
//	· 「先 io.ReadAll 再一次性发出」：请求还没发出去 ⇒ 对端永远等不到 ⇒
//	  读取器 5s 后报错 ⇒ 用例红。
//
// ⇒ 这条断言对「整文件缓冲」有牙齿（chunked 断言只能证明长度未知）。
func TestForwardUpload_TrulyLazy(t *testing.T) {
	peerStarted := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(peerStarted) })
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"file_count":1,"pickup_code":"AB12CD"}}`))
	}))
	defer srv.Close()

	c := NewDeliveryClient()
	res, err := c.ForwardUpload(context.Background(),
		conf.FederationDeliveryPeer{URL: srv.URL, Token: "shared-token"}, "tok-abc",
		[]DeliveryFile{{
			Name: "lazy.txt",
			Size: 4,
			Open: func() (io.ReadCloser, error) {
				return io.NopCloser(&blockingReader{wait: peerStarted, payload: "lazy"}), nil
			},
		}})
	require.NoError(t, err, "转发必须是边读边发（整文件缓冲会让对端等不到 body）")
	assert.Equal(t, "AB12CD", res.PickupCode)
}

// blockingReader 第一次 Read 前等 wait 通道关闭（超时 ⇒ 报错，说明不是流式）。
type blockingReader struct {
	wait    chan struct{}
	payload string
	primed  bool
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if !r.primed {
		select {
		case <-r.wait:
		case <-time.After(5 * time.Second):
			return 0, errors.New("对端未开始读取（转发不是流式：疑似先整文件缓冲了）")
		}
		r.primed = true
	}
	if len(p) == 0 {
		return 0, nil
	}
	n := copy(p, r.payload)
	r.payload = r.payload[n:]
	if len(r.payload) == 0 {
		return n, io.EOF
	}
	return n, nil
}
