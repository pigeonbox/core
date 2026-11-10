package opendal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/studio-b12/gowebdav"
)

// webdavDriver 基于 gowebdav 的 WebDAV 驱动（Nextcloud/坚果云/Alist/...）。
// key 以 URL 路径语义拼接（'/' 分隔），root 为远端子目录前缀（可为空）。
type webdavDriver struct {
	client *gowebdav.Client
	root   string
}

// newWebDAVDriver 构造 WebDAV 驱动。
// options:
//
//	url      必填；http(s)://host[:port]/base/
//	username / password  可选（Basic Auth）
//	root     可选远端子路径（如 "pigeonbox"），key 拼接到其后
func newWebDAVDriver(opts map[string]string) (*webdavDriver, error) {
	raw := strings.TrimSpace(opts["url"])
	if raw == "" {
		return nil, errors.New("webdav: url is required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("webdav: invalid url %q", raw)
	}
	client := gowebdav.NewClient(raw,
		strings.TrimSpace(opts["username"]), opts["password"])
	return &webdavDriver{client: client, root: strings.Trim(opts["root"], "/")}, nil
}

// abs key → 远端绝对路径
func (d *webdavDriver) abs(key string) string {
	return "/" + path.Join(d.root, key)
}

// isNotExist 把 gowebdav 的 404 归一判断（gowebdav 未导出结构化错误类型，
// 以错误文本判断 + 标准库 os.ErrNotExist 双保险）。
func (d *webdavDriver) isNotExist(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return strings.Contains(err.Error(), "404")
}

func (d *webdavDriver) notExistWrap(key string, err error) error {
	if d.isNotExist(err) {
		return fmt.Errorf("%w: webdav %s", os.ErrNotExist, d.abs(key))
	}
	return err
}

func (d *webdavDriver) Write(ctx context.Context, key string, data []byte) error {
	if err := d.client.MkdirAll(path.Dir(d.abs(key)), 0755); err != nil {
		return err
	}
	return d.client.Write(d.abs(key), data, 0644)
}

func (d *webdavDriver) WriteStream(ctx context.Context, key string, r io.Reader, size int64) error {
	if err := d.client.MkdirAll(path.Dir(d.abs(key)), 0755); err != nil {
		return err
	}
	return d.client.WriteStream(d.abs(key), r, 0644)
}

func (d *webdavDriver) Read(ctx context.Context, key string) ([]byte, error) {
	// gowebdav.Client.Read 内部整读无上限（core#118 同族）⇒ 改走 ReadStream +
	// 限量整读（404 归一语义不变）。
	rc, err := d.client.ReadStream(d.abs(key))
	if err != nil {
		return nil, d.notExistWrap(key, err)
	}
	defer func() { _ = rc.Close() }()
	return ReadAllLimited(rc, MaxWholeReadBytes)
}

func (d *webdavDriver) Reader(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := d.client.ReadStream(d.abs(key))
	if err != nil {
		return nil, d.notExistWrap(key, err)
	}
	return rc, nil
}

func (d *webdavDriver) Stat(ctx context.Context, key string) (*Metadata, error) {
	info, err := d.client.Stat(d.abs(key))
	if err != nil {
		return nil, d.notExistWrap(key, err)
	}
	return &Metadata{
		Path:    key,
		Size:    info.Size(),
		IsDir:   info.IsDir(),
		ModTime: info.ModTime(),
	}, nil
}

func (d *webdavDriver) Delete(ctx context.Context, key string) error {
	err := d.client.Remove(d.abs(key))
	if err != nil {
		return d.notExistWrap(key, err)
	}
	return nil
}

// RemoveAll 递归删除目录（gowebdav.RemoveAll 按 DELETE 集合语义递归）。
func (d *webdavDriver) RemoveAll(ctx context.Context, key string) error {
	if strings.Trim(key, "/") == "" {
		return errors.New("webdav: refusing RemoveAll with empty key")
	}
	err := d.client.RemoveAll(d.abs(key))
	if err != nil && !d.isNotExist(err) {
		return err
	}
	return nil
}

// MkdirAll 创建远端目录（含父目录）
func (d *webdavDriver) MkdirAll(ctx context.Context, key string) error {
	return d.client.MkdirAll(d.abs(key), 0755)
}

func (d *webdavDriver) List(ctx context.Context, key string) ([]*Metadata, error) {
	fis, err := d.client.ReadDir(d.abs(key))
	if err != nil {
		return nil, d.notExistWrap(key, err)
	}
	out := make([]*Metadata, 0, len(fis))
	for _, fi := range fis {
		out = append(out, &Metadata{
			Path:    fi.Name(),
			Size:    fi.Size(),
			IsDir:   fi.IsDir(),
			ModTime: fi.ModTime(),
		})
	}
	return out, nil
}

// Probe 认证级连通性验证：根路径可达（401/网络错误会在此暴露）。
func (d *webdavDriver) Probe(ctx context.Context) error {
	if _, err := d.client.Stat(d.abs("")); err != nil {
		return fmt.Errorf("webdav 根路径不可达: %w", err)
	}
	return nil
}
