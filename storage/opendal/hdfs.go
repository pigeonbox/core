package opendal

import (
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
	"time"
)

// hdfsDriver WebHDFS REST 驱动（NameNode http(s) 端口，默认 9870）。
// 认证：简单代理用户（user.name 查询参数）；Kerberos 为扩展点未实现。
// CREATE 两步走用 noredirect=true 拿 Location 后直 PUT（免去重定向方法改写）。
type hdfsDriver struct {
	endpoint string // http(s)://namenode:port
	user     string
	root     string
	client   *http.Client
}

// newHDFSDriver 构造 WebHDFS 驱动。
// options: endpoint（必填）/user/root
func newHDFSDriver(opts map[string]string) (*hdfsDriver, error) {
	ep := strings.TrimRight(strings.TrimSpace(opts["endpoint"]), "/")
	if ep == "" || !strings.HasPrefix(ep, "http") {
		return nil, errors.New("hdfs: endpoint 必填（WebHDFS 根地址，如 http://namenode:9870）")
	}
	return &hdfsDriver{
		endpoint: ep,
		user:     strings.TrimSpace(opts["user"]),
		root:     strings.Trim(opts["root"], "/"),
		client:   &http.Client{Timeout: 5 * time.Minute},
	}, nil
}

// abs key → HDFS 绝对路径
func (d *hdfsDriver) abs(key string) string {
	return "/" + path.Join(d.root, key)
}

// opURL 组装 WebHDFS 操作 URL
func (d *hdfsDriver) opURL(hdfsPath, op string, extra map[string]string) string {
	u, _ := url.Parse(d.endpoint + "/webhdfs/v1" + path.Join("/", hdfsPath))
	q := u.Query()
	q.Set("op", op)
	if d.user != "" {
		q.Set("user.name", d.user)
	}
	for k, v := range extra {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// do 执行请求；noredirect 的两步走（CREATE/OPEN）由调用方处理。
func (d *hdfsDriver) do(ctx context.Context, method, u string, body io.Reader) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = body
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	return d.client.Do(req)
}

// remoteError 解 WebHDFS RemoteException 的 404/权限语义
func (d *hdfsDriver) remoteError(key string, status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if status == http.StatusNotFound || strings.Contains(msg, "FileNotFoundException") {
		return fmt.Errorf("%w: hdfs %s", os.ErrNotExist, d.abs(key))
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("hdfs %s: %d %s（检查 user.name/权限）", key, status, msg)
	}
	return fmt.Errorf("hdfs %s: %d %s", key, status, msg)
}

func (d *hdfsDriver) Write(ctx context.Context, key string, data []byte) error {
	return d.WriteStream(ctx, key, strings.NewReader(string(data)), int64(len(data)))
}

func (d *hdfsDriver) WriteStream(ctx context.Context, key string, r io.Reader, _ int64) error {
	// 第一步：noredirect 拿 datanode Location
	u := d.opURL(d.abs(key), "CREATE", map[string]string{
		"noredirect": "true", "overwrite": "true",
		"permission": "644", "blocksize": "134217728",
	})
	resp, err := d.do(ctx, http.MethodPut, u, nil)
	if err != nil {
		return err
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return d.remoteError(key, resp.StatusCode, b)
	}
	var loc struct {
		Location string `json:"Location"`
	}
	if err := json.Unmarshal(b, &loc); err != nil || loc.Location == "" {
		return fmt.Errorf("hdfs CREATE 未返回 Location: %s", string(b))
	}
	// 第二步：PUT 数据到 datanode
	resp2, err := d.do(ctx, http.MethodPut, loc.Location, r)
	if err != nil {
		return err
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode >= 300 {
		b2, _ := io.ReadAll(io.LimitReader(resp2.Body, 1<<12))
		return fmt.Errorf("hdfs 写入 %s: %d %s", key, resp2.StatusCode, string(b2))
	}
	return nil
}

func (d *hdfsDriver) Read(ctx context.Context, key string) ([]byte, error) {
	rc, err := d.Reader(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

func (d *hdfsDriver) Reader(ctx context.Context, key string) (io.ReadCloser, error) {
	u := d.opURL(d.abs(key), "OPEN", map[string]string{"noredirect": "true"})
	resp, err := d.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, d.remoteError(key, resp.StatusCode, b)
	}
	var loc struct {
		Location string `json:"Location"`
	}
	if err := json.Unmarshal(b, &loc); err != nil || loc.Location == "" {
		return nil, fmt.Errorf("hdfs OPEN 未返回 Location: %s", string(b))
	}
	resp2, err := d.do(ctx, http.MethodGet, loc.Location, nil)
	if err != nil {
		return nil, err
	}
	if resp2.StatusCode >= 300 {
		_ = resp2.Body.Close()
		return nil, fmt.Errorf("hdfs 读取 %s: %d", key, resp2.StatusCode)
	}
	return resp2.Body, nil
}

// fileStatus WebHDFS FileStatus 结构
type hdfsFileStatus struct {
	PathSuffix       string `json:"pathSuffix"`
	Type             string `json:"type"`
	Length           int64  `json:"length"`
	ModificationTime int64  `json:"modificationTime"`
}

func (d *hdfsDriver) Stat(ctx context.Context, key string) (*Metadata, error) {
	resp, err := d.do(ctx, http.MethodGet, d.opURL(d.abs(key), "GETFILESTATUS", nil), nil)
	if err != nil {
		return nil, err
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, d.remoteError(key, resp.StatusCode, b)
	}
	var parsed struct {
		FileStatus hdfsFileStatus `json:"FileStatus"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, err
	}
	fs := parsed.FileStatus
	return &Metadata{
		Path:    key,
		Size:    fs.Length,
		IsDir:   fs.Type == "DIRECTORY",
		ModTime: time.UnixMilli(fs.ModificationTime),
	}, nil
}

func (d *hdfsDriver) Delete(ctx context.Context, key string) error {
	resp, err := d.do(ctx, http.MethodDelete, d.opURL(d.abs(key), "DELETE", map[string]string{"recursive": "false"}), nil)
	if err != nil {
		return err
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return d.remoteError(key, resp.StatusCode, b)
	}
	return nil
}

func (d *hdfsDriver) RemoveAll(ctx context.Context, key string) error {
	if strings.Trim(key, "/") == "" {
		return errors.New("hdfs: refusing RemoveAll with empty key")
	}
	resp, err := d.do(ctx, http.MethodDelete, d.opURL(d.abs(key), "DELETE", map[string]string{"recursive": "true"}), nil)
	if err != nil {
		return err
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return d.remoteError(key, resp.StatusCode, b)
	}
	return nil
}

func (d *hdfsDriver) MkdirAll(ctx context.Context, key string) error {
	resp, err := d.do(ctx, http.MethodPut, d.opURL(d.abs(key), "MKDIRS", map[string]string{"permission": "755"}), nil)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("hdfs MKDIRS %s: %d", key, resp.StatusCode)
	}
	return nil
}

func (d *hdfsDriver) List(ctx context.Context, key string) ([]*Metadata, error) {
	resp, err := d.do(ctx, http.MethodGet, d.opURL(d.abs(key), "LISTSTATUS", nil), nil)
	if err != nil {
		return nil, err
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	_ = resp.Body.Close()
	// ⚠️ **不存在的目录 ⇒ 空列表，而非错误**（2026-11-02 真机实测修）：
	// WebHDFS 对不存在的路径返回 404 FileNotFoundException，而 S3/Azure 对
	// 「前缀不存在」的语义是**空列表**（对象存储无真目录）⇒ 同一条 List 调用
	// 在 S3 与 HDFS 上行为不一致。
	// 影响面是实的：后台 janitor 用 ListRemoteObjects("uploads/") 清理 presign
	// 孤儿 ⇒ HDFS 后端下「uploads/ 尚不存在」会让整个清理任务报错退出
	// （而 S3 后端正常返回空集、无事可做）⇒ 孤儿清理在 HDFS 上不可用。
	if resp.StatusCode == http.StatusNotFound {
		return []*Metadata{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, d.remoteError(key, resp.StatusCode, b)
	}
	var parsed struct {
		FileStatuses struct {
			FileStatus []hdfsFileStatus `json:"FileStatus"`
		} `json:"FileStatuses"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, err
	}
	out := make([]*Metadata, 0, len(parsed.FileStatuses.FileStatus))
	for _, fs := range parsed.FileStatuses.FileStatus {
		out = append(out, &Metadata{
			Path:    fs.PathSuffix,
			Size:    fs.Length,
			IsDir:   fs.Type == "DIRECTORY",
			ModTime: time.UnixMilli(fs.ModificationTime),
		})
	}
	return out, nil
}

// Probe 认证级连通性验证：根目录 LISTSTATUS 可达。
func (d *hdfsDriver) Probe(ctx context.Context) error {
	resp, err := d.do(ctx, http.MethodGet, d.opURL("/", "LISTSTATUS", nil), nil)
	if err != nil {
		return fmt.Errorf("hdfs 不可达: %w", err)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return d.remoteError("/", resp.StatusCode, b)
	}
	return nil
}
