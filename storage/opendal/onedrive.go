package opendal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/oekit/concurrency/retry"
)

// onedriveDriver OneDrive / SharePoint（Microsoft Graph）驱动，零 SDK 依赖。
//
// 授权模型（自托管友好）：在 Azure AD 注册应用（Files.ReadWrite.All + offline_access，
// 公共客户端或机密客户端均可），用户走一次授权码流程获得 refresh_token 后配置至此；
// 运行时用 refresh_token 换取 access_token（服务端自动续期并滚动 refresh_token）。
//
// 写入：≤4MB 简单上传；更大走 upload session 分片（Driver.WriteStream 有确切 size，天然匹配）。
// 读取：GET /content 302 到 CDN，http 客户端跟随，流式返回。
type onedriveDriver struct {
	clientID     string
	clientSecret string
	refreshToken string
	tenant       string
	driveID      string // 空 = me/drive
	root         string
	client       *http.Client

	mu          sync.Mutex
	accessToken string
	tokenExp    time.Time
	// 新 refresh_token（滚动续期；是否落盘由上层存储配置域决定，v1 仅内存复用）
	newRefreshToken string
}

// newOneDriveDriver 构造 OneDrive 驱动。
// options: client_id/client_secret/refresh_token（三项必填）/tenant/drive_id/root
func newOneDriveDriver(opts map[string]string) (*onedriveDriver, error) {
	clientID := strings.TrimSpace(opts["client_id"])
	clientSecret := strings.TrimSpace(opts["client_secret"])
	refreshToken := strings.TrimSpace(opts["refresh_token"])
	if clientID == "" || clientSecret == "" || refreshToken == "" {
		return nil, errors.New("onedrive: client_id/client_secret/refresh_token 必填（授权流程见存储文档）")
	}
	tenant := strings.TrimSpace(opts["tenant"])
	if tenant == "" {
		tenant = "common"
	}
	return &onedriveDriver{
		clientID:     clientID,
		clientSecret: clientSecret,
		refreshToken: refreshToken,
		tenant:       tenant,
		driveID:      strings.TrimSpace(opts["drive_id"]),
		root:         strings.Trim(opts["root"], "/"),
		client:       &http.Client{Timeout: 15 * time.Minute},
	}, nil
}

// driveBase /drives/{id} 或 /me/drive
func (d *onedriveDriver) driveBase() string {
	if d.driveID != "" {
		return "https://graph.microsoft.com/v1.0/drives/" + d.driveID
	}
	return "https://graph.microsoft.com/v1.0/me/drive"
}

// abs key → item path 语义（root:/{path}:）
func (d *onedriveDriver) itemPath(key string) string {
	p := path.Join("/", d.root, key)
	if p == "/" {
		return "/root"
	}
	return "/root:" + p + ":"
}

// getToken 取 access token（带缓存与 401 后强制刷新）
func (d *onedriveDriver) getToken(ctx context.Context, forceRefresh bool) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !forceRefresh && d.accessToken != "" && time.Now().Before(d.tokenExp.Add(-2*time.Minute)) {
		return d.accessToken, nil
	}
	form := url.Values{}
	form.Set("client_id", d.clientID)
	form.Set("client_secret", d.clientSecret)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", d.refreshToken)
	form.Set("scope", "Files.ReadWrite.All offline_access")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", d.tenant),
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := d.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("onedrive token 请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var parsed struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	if parsed.AccessToken == "" {
		return "", fmt.Errorf("onedrive token 刷新失败(%s): %s", parsed.Error, parsed.ErrorDescription)
	}
	d.accessToken = parsed.AccessToken
	d.tokenExp = time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	if parsed.RefreshToken != "" {
		d.refreshToken = parsed.RefreshToken // 滚动续期
		d.newRefreshToken = parsed.RefreshToken
	}
	return d.accessToken, nil
}

// apiReq 带鉴权的 Graph 请求；401 时强刷 token 重试一次。
func (d *onedriveDriver) apiReq(ctx context.Context, method, u string, body io.Reader, hdr map[string]string) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := d.getToken(ctx, attempt > 0)
		if err != nil {
			return nil, err
		}
		var rd io.Reader
		if body != nil && attempt == 0 {
			rd = body
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := d.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			_ = resp.Body.Close()
			continue
		}
		return resp, nil
	}
	return nil, errors.New("onedrive: unreachable")
}

func (d *onedriveDriver) errFromResp(key string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
	msg := string(b)
	var ge struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &ge)
	if ge.Error.Message != "" {
		msg = ge.Error.Message
	}
	if ge.Error.Code == "itemNotFound" || resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: onedrive %s", os.ErrNotExist, key)
	}
	return fmt.Errorf("onedrive %s: %d %s", key, resp.StatusCode, msg)
}

func (d *onedriveDriver) Write(ctx context.Context, key string, data []byte) error {
	return d.WriteStream(ctx, key, bytes.NewReader(data), int64(len(data)))
}

func (d *onedriveDriver) WriteStream(ctx context.Context, key string, r io.Reader, size int64) error {
	// ≤4MB 简单上传
	if size <= 4*1024*1024 {
		resp, err := d.apiReq(ctx, http.MethodPut, d.driveBase()+d.itemPath(key)+"/content", r,
			map[string]string{"Content-Type": "application/octet-stream"})
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 300 {
			return d.errFromResp(key, resp)
		}
		return nil
	}

	// 大文件：upload session 分片（Graph 约定 5-10MB 切片，每片可重试）
	const chunk = 8 * 1024 * 1024
	sessURL := d.driveBase() + d.itemPath(key) + "/createUploadSession"
	resp, err := d.apiReq(ctx, http.MethodPost, sessURL, bytes.NewReader([]byte(`{"item": {"@microsoft.graph.conflictBehavior": "replace"}}`)),
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	var sess struct {
		UploadURL string `json:"uploadUrl"`
	}
	err = json.NewDecoder(resp.Body).Decode(&sess)
	_ = resp.Body.Close()
	if err != nil || sess.UploadURL == "" {
		return fmt.Errorf("onedrive 会话创建失败: %v", err)
	}

	offset := int64(0)
	for offset < size {
		n := int64(chunk)
		if remain := size - offset; remain < n {
			n = remain
		}
		end := offset + n - 1
		// 每片独立请求（可重试语义）
		// 顺序读出本片（WriteStream 语义为顺序写入；重试复用同一缓冲）
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return fmt.Errorf("onedrive 读取分片 %d: %w", offset, err)
		}
		// 每片独立请求，至多 3 次（网络错误/非 2xx 重试；零延迟与原实现一致）
		err := retry.Do(ctx, retry.Config{Attempts: 3}, func(attempt int) error {
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, sess.UploadURL, bytes.NewReader(buf))
			if err != nil {
				return err
			}
			req.ContentLength = n
			req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, end, size))
			resp, err := d.client.Do(req)
			if err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
				return nil
			}
			// 会话过期则整体失败（v1 不做会话重建）
			return fmt.Errorf("onedrive 分片上传 %s: %d", key, resp.StatusCode)
		})
		if err != nil {
			return err
		}
		offset += n
	}
	return nil
}

func (d *onedriveDriver) Read(ctx context.Context, key string) ([]byte, error) {
	rc, err := d.Reader(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

func (d *onedriveDriver) Reader(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := d.apiReq(ctx, http.MethodGet, d.driveBase()+d.itemPath(key)+"/content", nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: onedrive %s", os.ErrNotExist, key)
	}
	if resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, d.errFromResp(key, resp)
	}
	return resp.Body, nil
}

func (d *onedriveDriver) Stat(ctx context.Context, key string) (*Metadata, error) {
	resp, err := d.apiReq(ctx, http.MethodGet, d.driveBase()+d.itemPath(key), nil, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return nil, d.errFromResp(key, resp)
	}
	var item struct {
		Size                 int64     `json:"size"`
		LastModifiedDateTime string    `json:"lastModifiedDateTime"`
		Folder               *struct{} `json:"folder"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return nil, err
	}
	mt, _ := time.Parse(time.RFC3339, item.LastModifiedDateTime)
	return &Metadata{Path: key, Size: item.Size, IsDir: item.Folder != nil, ModTime: mt}, nil
}

func (d *onedriveDriver) Delete(ctx context.Context, key string) error {
	resp, err := d.apiReq(ctx, http.MethodDelete, d.driveBase()+d.itemPath(key), nil, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: onedrive %s", os.ErrNotExist, key)
	}
	if resp.StatusCode >= 300 {
		return d.errFromResp(key, resp)
	}
	return nil
}

// RemoveAll 递归删除（Graph DELETE 目录即递归）
func (d *onedriveDriver) RemoveAll(ctx context.Context, key string) error {
	if strings.Trim(key, "/") == "" {
		return errors.New("onedrive: refusing RemoveAll with empty key")
	}
	return d.Delete(ctx, key)
}

func (d *onedriveDriver) List(ctx context.Context, key string) ([]*Metadata, error) {
	resp, err := d.apiReq(ctx, http.MethodGet, d.driveBase()+d.itemPath(key)+"/children", nil, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return nil, d.errFromResp(key, resp)
	}
	var parsed struct {
		Value []struct {
			Name                 string    `json:"name"`
			Size                 int64     `json:"size"`
			LastModifiedDateTime string    `json:"lastModifiedDateTime"`
			Folder               *struct{} `json:"folder"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	out := make([]*Metadata, 0, len(parsed.Value))
	for _, v := range parsed.Value {
		mt, _ := time.Parse(time.RFC3339, v.LastModifiedDateTime)
		out = append(out, &Metadata{Path: v.Name, Size: v.Size, IsDir: v.Folder != nil, ModTime: mt})
	}
	return out, nil
}

// Probe 认证级连通性验证：token 可换 + drive 可读（401/凭证错在此暴露）。
func (d *onedriveDriver) Probe(ctx context.Context) error {
	if _, err := d.getToken(ctx, true); err != nil {
		return err
	}
	resp, err := d.apiReq(ctx, http.MethodGet, d.driveBase()+"?select=id", nil, nil)
	if err != nil {
		return fmt.Errorf("onedrive drive 不可达: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return d.errFromResp("drive", resp)
	}
	return nil
}
