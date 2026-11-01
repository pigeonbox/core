// delivery.go — 跨站投递（联邦前台化，2026-11-01）。
//
// 形态（设计稿：docs/specs/2026-11-01-federation-frontend-delivery-design.md）：
//
//	访客 ──① 在发起柜（A 柜）前台填「对方柜 URL」+ 选文件
//	        A 柜 ──② 校验目标 ∈ 管理员登记的「可投递对端」
//	        A 柜 ──③ 带共享口令调对方柜（B 柜）铸投递槽位
//	        A 柜 ──④ 把文件**流式**转发到 B 柜的访客投递端点
//	                B 柜落库 ⇒ **B 柜的 share 域铸 6 位取件码**
//	        A 柜 ──⑤ 把 B 的 6 位码回给访客（访客转告取件人）
//	取件人 ──⑥ 在 **B 柜** 用该 6 位码取件
//
// 三条不变性（改代码前先读）：
//
//  1. **码由对方柜铸**：A 柜只透传 B 柜返回的 6 位码 ⇒ 码空间天然是对方的
//     （取件人站在对方柜取件）⇒ 不需要跨柜共享码空间。
//  2. **出站 URL 只来自 admin 配置**（conf.FederationConfig.DeliveryPeers），
//     不是访客任意输入 ⇒ SSRF 面从「任意 URL」收窄到「admin 配的 URL」；
//     私网对端按**对端粒度**放行（allow_private），不动全局 SSRF 开关。
//  3. **口令即权限**（同现有联邦的「口令 bearer」信任级）：比对用常数时间
//     比较，防时序侧信道。
//
// 归属：本文件属联邦域（跨实例协作），出站面与铸槽位面都在这里；
// 访客投递的重管道（multipart 解析）留在 transport/http/handler 层。
package federation

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/oekit/concurrency/async"
	"github.com/pigeonbox/core/conf"
)

const (
	// DeliveryTokenHeader 共享口令 bearer 的请求头名（A 柜 → B 柜）。
	DeliveryTokenHeader = "X-Federation-Token"

	// deliverySlotPath 对端铸投递槽位的端点（publicapi 域，公开面）。
	deliverySlotPath = "/api/v1/federation/delivery-slot"
	// deliveryUploadPath 对端访客投递的端点（寄件码域，公开面）。
	deliveryUploadPath = "/api/v1/request/%s/upload"

	// deliverySlotTimeout 铸槽位的出站超时（秒级交互，不等大文件）。
	deliverySlotTimeout = 10 * time.Second
	// deliveryForwardTimeout 转发文件的上限超时（兜底；正常由入站 ctx 收敛）。
	deliveryForwardTimeout = 30 * time.Minute
	// deliveryMaxRespBytes 对端响应体的读取上限（防对端回大 body 撑内存）。
	deliveryMaxRespBytes = 64 << 10
)

// DeliveryFile 待转发的一个文件。
//
// Open 惰性打开（转发时才打开文件句柄）⇒ 调用方可以在**解析完表单之后**
// 再决定是否转发（未登记对端 ⇒ 一个出站请求都不发）。
type DeliveryFile struct {
	// Name 原始文件名（转发时作为 multipart 的 filename）。
	Name string
	// Size 申报大小（字节；仅用于上限预检，不参与内容判断）。
	Size int64
	// Open 打开内容读取器（调用方负责 Close）。
	Open func() (io.ReadCloser, error)
}

// DeliveryResult 转发结果（对方柜铸的取件码）。
type DeliveryResult struct {
	// FileCount 对方柜落盘的文件数。
	FileCount int
	// PickupCode 对方柜铸的 6 位取件码（**在对方柜使用**）。
	PickupCode string
}

// DeliveryPeers 当前生效的可投递对端（conf 快照读）。
//
// 读 conf 快照（copy-on-write 原子换指针）⇒ 配置热应用（管理台保存）后
// 立即生效，且不会读到撕裂的字段组合。
func DeliveryPeers() []conf.FederationDeliveryPeer {
	cfg := conf.Snapshot()
	if cfg == nil {
		return nil
	}
	return cfg.Federation.DeliveryPeers
}

// NormalizePeerURL 归一化对端基址（scheme/host 小写、去默认端口、去尾斜杠、
// 去 query/fragment）。
//
// 归一化的意义：访客在前台填的 URL 与管理员登记的 URL 只要指向同一个基址
// 就该命中（"http://B:12345/" ≡ "http://b:12345"）⇒ 否则访客会因为多打一个
// 斜杠而投递失败。
//
// ⚠️ 不接受带 userinfo（http://user:pass@host/）的基址：凭证不该出现在
// 「对方柜地址」里，且它会让 URL 形态判据变得含糊。
func NormalizePeerURL(rawURL string) (string, error) {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return "", errors.New("对方柜地址不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("对方柜地址解析失败: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("对方柜地址仅支持 http/https，当前为 %q", u.Scheme)
	}
	if u.User != nil {
		return "", errors.New("对方柜地址不得携带用户名/口令")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", errors.New("对方柜地址缺少 host")
	}
	if strings.Contains(host, ":") { // IPv6 字面量
		host = "[" + host + "]"
	}
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = "" // 默认端口归一化掉
	}
	if port != "" {
		host += ":" + port
	}
	path := strings.TrimRight(u.Path, "/")
	return u.Scheme + "://" + host + path, nil
}

// MatchDeliveryPeer 按归一化后的基址匹配已登记的对端。
//
// ⚠️ **必须命中**才允许投递（未命中 ⇒ 明确拒绝且**不发任何出站请求**）：
// 访客填一个未登记的 URL 时，A 柜当场拒，而不是「发出去后发现错」。
func MatchDeliveryPeer(rawURL string) (conf.FederationDeliveryPeer, bool) {
	target, err := NormalizePeerURL(rawURL)
	if err != nil {
		return conf.FederationDeliveryPeer{}, false
	}
	for _, p := range DeliveryPeers() {
		base, err := NormalizePeerURL(p.URL)
		if err != nil {
			continue // 配置里的坏地址直接跳过（保存时已校验过）
		}
		if base == target {
			return p, true
		}
	}
	return conf.FederationDeliveryPeer{}, false
}

// VerifyDeliveryToken 比对共享口令（常数时间）。
//
// 语义：**任一**登记对端的口令匹配即通过（不要求知道调用方是谁——口令即
// 权限，与现有联邦的「口令 bearer」信任级一致；双向登记的前提由
// 「A 只投给登记过的 B」保证）。
//
// ⚠️ 用 subtle.ConstantTimeCompare（常数时间）而不是 ==：普通字符串比较
// 会因首个不同字节的位置不同而耗时不同 ⇒ 可被时序侧信道逐字节试探口令。
// 循环里**不提前 break**（命中后继续比完），避免「命中位置」也变成时序信号。
func VerifyDeliveryToken(got string) bool {
	if got == "" {
		return false
	}
	peers := DeliveryPeers()
	if len(peers) == 0 {
		return false
	}
	ok := false
	for _, p := range peers {
		want := strings.TrimSpace(p.Token)
		if want == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			ok = true
		}
	}
	return ok
}

// DeliveryClient 跨站投递出站客户端（无条件构造；与联邦是否启用无关——
// 投递面是「柜子之间」的通道，不依赖联邦运行时）。
type DeliveryClient struct {
	// HTTPClient 出站 HTTP 客户端（nil = 用默认构造）。
	HTTPClient *http.Client
}

// NewDeliveryClient 构造出站客户端。
//
// ⚠️ **不跟随重定向**：对端回一个 302 到内网地址就能借本站的出站面绕过
// SSRF 判定（出站 URL 虽受控，但重定向目标不受控）⇒ 3xx 一律当失败。
func NewDeliveryClient() *DeliveryClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &DeliveryClient{HTTPClient: &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (c *DeliveryClient) httpClient() *http.Client {
	if c != nil && c.HTTPClient != nil {
		return c.HTTPClient
	}
	return NewDeliveryClient().HTTPClient
}

// CreateSlot 向对端铸一个访客投递槽位（口令 bearer 授权）。
//
// 返回对端铸的**寄件码 token**（投递链接令牌；不是 6 位取件码——此时还没有
// 文件，对方柜不可能铸出取件码）。
func (c *DeliveryClient) CreateSlot(ctx context.Context, peer conf.FederationDeliveryPeer, fromName, note, title string) (string, error) {
	base, err := NormalizePeerURL(peer.URL)
	if err != nil {
		return "", err
	}
	form := url.Values{}
	if v := strings.TrimSpace(fromName); v != "" {
		form.Set("from_name", v)
	}
	if v := strings.TrimSpace(note); v != "" {
		form.Set("note", v)
	}
	if v := strings.TrimSpace(title); v != "" {
		form.Set("title", v)
	}

	reqCtx, cancel := context.WithTimeout(ctx, deliverySlotTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		base+deliverySlotPath, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(DeliveryTokenHeader, peer.Token)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("对方柜铸槽位失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, deliveryMaxRespBytes))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("对方柜拒绝铸槽位（HTTP %d）", resp.StatusCode)
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    *struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("对方柜响应解析失败: %w", err)
	}
	if out.Code != 0 {
		msg := out.Message
		if msg == "" {
			msg = "对方柜拒绝了铸槽位请求"
		}
		return "", fmt.Errorf("%s", msg)
	}
	if out.Data == nil || out.Data.Token == "" {
		return "", errors.New("对方柜未返回投递令牌")
	}
	return out.Data.Token, nil
}

// ForwardUpload 把文件**流式**转发到对端的访客投递端点。
//
// ⚠️ **不得整文件读进内存**：写侧 goroutine 用 io.Pipe + multipart.Writer +
// io.Copy 把文件内容一段段推进管道，请求体因此是 **chunked**（长度未知）
// ⇒ 内存占用与文件大小无关（A 柜只是中转，不落盘、不缓冲）。
//
// 返回对方柜铸的 6 位取件码（A 柜只透传，不铸码）。
func (c *DeliveryClient) ForwardUpload(ctx context.Context, peer conf.FederationDeliveryPeer, token string, files []DeliveryFile) (*DeliveryResult, error) {
	if len(files) == 0 {
		return nil, errors.New("没有可转发的文件")
	}
	base, err := NormalizePeerURL(peer.URL)
	if err != nil {
		return nil, err
	}
	target := base + fmt.Sprintf(deliveryUploadPath, url.PathEscape(token))

	forwardCtx, cancel := context.WithTimeout(ctx, deliveryForwardTimeout)
	defer cancel()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	contentType := mw.FormDataContentType()

	writeErr := make(chan error, 1)
	// ⚠️ 用 async.GoSafe（仓内机械守卫：禁裸 go 语句）——panic 经全局
	// async.SetPanicHandler 落日志 + stack，不静默带走整个进程。
	//
	// 写侧无论正常结束还是 panic 收编，**都必须**给等待方一个结果
	// （defer 里投递）⇒ 否则调用方会死等。
	async.GoSafe(func() {
		var werr error
		defer func() {
			if werr != nil {
				_ = pw.CloseWithError(werr)
			} else {
				_ = pw.Close()
			}
			writeErr <- werr
		}()
		werr = writeDeliveryBody(mw, files)
	})

	req, err := http.NewRequestWithContext(forwardCtx, http.MethodPost, target, pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// 关掉读端 ⇒ 写侧 goroutine 立刻拿到 ErrClosedPipe 退出（不漏 goroutine）。
		_ = pr.CloseWithError(err)
		werr := <-writeErr
		if werr != nil {
			return nil, fmt.Errorf("转发到对方柜失败: %w", werr)
		}
		return nil, fmt.Errorf("转发到对方柜失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	defer func() { _ = pr.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, deliveryMaxRespBytes))
	// 收写侧结果：正常情况下对端读完整个 body 才应答 ⇒ 写侧已结束。
	var werr error
	select {
	case werr = <-writeErr:
	case <-time.After(5 * time.Second):
		werr = errors.New("转发写侧未在预期时间内结束")
	}
	if resp.StatusCode != http.StatusOK {
		msg := peerErrorMessage(body)
		if msg == "" {
			msg = fmt.Sprintf("对方柜收件失败（HTTP %d）", resp.StatusCode)
		}
		return nil, errors.New(msg)
	}
	if werr != nil {
		return nil, fmt.Errorf("转发到对方柜失败: %w", werr)
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    *struct {
			FileCount  int    `json:"file_count"`
			PickupCode string `json:"pickup_code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("对方柜响应解析失败: %w", err)
	}
	if out.Code != 0 {
		msg := out.Message
		if msg == "" {
			msg = "对方柜拒绝了投递"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	res := &DeliveryResult{}
	if out.Data != nil {
		res.FileCount = out.Data.FileCount
		res.PickupCode = out.Data.PickupCode
	}
	if res.FileCount == 0 {
		res.FileCount = len(files)
	}
	return res, nil
}

// writeDeliveryBody 把待转发文件逐个写进 multipart（**流式**：io.Copy 逐段推进）。
func writeDeliveryBody(mw *multipart.Writer, files []DeliveryFile) error {
	for _, f := range files {
		w, err := mw.CreateFormFile("files", f.Name)
		if err != nil {
			return err
		}
		if f.Open == nil {
			return fmt.Errorf("文件 %s 缺少读取器", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		_, cerr := io.Copy(w, rc)
		_ = rc.Close()
		if cerr != nil {
			return cerr
		}
	}
	return mw.Close()
}

// peerErrorMessage 从对端错误响应里取中文文案（取不到返回空串）。
func peerErrorMessage(body []byte) string {
	var out struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return ""
	}
	return strings.TrimSpace(out.Message)
}
