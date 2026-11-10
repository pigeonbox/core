package opendal

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"
)

// tlsConfigFor 显式 FTPS（AUTH TLS）的 TLS 配置；SNI 取 host（去端口）。
func tlsConfigFor(hostPort string) *tls.Config {
	host := hostPort
	if i := strings.LastIndex(hostPort, ":"); i > 0 {
		host = hostPort[:i]
	}
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

// ftpDriver 基于 jlaffaye/ftp 的 FTP/FTPS 驱动。
// 连接按操作建立（FTP 控制连接单线程，短连接最稳，避免陈旧连接与并发争用）。
// key 以 '/' 路径语义拼接，root 为远端子目录前缀（可为空）。
type ftpDriver struct {
	host     string // host:port
	username string
	password string
	useTLS   bool
	root     string
	timeout  time.Duration
}

// newFTPDriver 构造 FTP 驱动。
// options:
//
//	host          必填；host[:port]（port 缺省 21）
//	username / password  可选（匿名可不填）
//	tls           "true" = 显式 FTPS（AUTH TLS）
//	root          可选远端子目录
func newFTPDriver(opts map[string]string) (*ftpDriver, error) {
	host := strings.TrimSpace(opts["host"])
	if host == "" {
		return nil, errors.New("ftp: host is required")
	}
	if !strings.Contains(host, ":") {
		host += ":21"
	}
	return &ftpDriver{
		host:     host,
		username: opts["username"],
		password: opts["password"],
		useTLS:   opts["tls"] == "true",
		root:     strings.Trim(opts["root"], "/"),
		timeout:  30 * time.Second,
	}, nil
}

// connect 建立连接并登录，切到 root 子目录。
func (d *ftpDriver) connect() (*ftp.ServerConn, error) {
	var opts []ftp.DialOption
	opts = append(opts, ftp.DialWithTimeout(d.timeout))
	if d.useTLS {
		opts = append(opts, ftp.DialWithTLS(tlsConfigFor(d.host)))
	}
	conn, err := ftp.Dial(d.host, opts...)
	if err != nil {
		return nil, fmt.Errorf("ftp dial %s: %w", d.host, err)
	}
	if err := conn.Login(d.username, d.password); err != nil {
		_ = conn.Quit()
		return nil, fmt.Errorf("ftp login: %w", err)
	}
	if d.root != "" {
		if err := conn.ChangeDir(d.root); err != nil {
			_ = conn.Quit()
			return nil, fmt.Errorf("ftp ChangeDir %s: %w", d.root, err)
		}
	}
	return conn, nil
}

// abs key → 相对登录目录的远端路径
func (d *ftpDriver) abs(key string) string {
	return path.Join("/", d.root, key)
}

// mkdirAllRemote 逐段建目录（FTP 无递归 mkdir；已存在返回 550，忽略）。
func (d *ftpDriver) mkdirAllRemote(conn *ftp.ServerConn, dir string) {
	if dir == "" || dir == "/" {
		return
	}
	cur := ""
	for _, seg := range strings.Split(strings.Trim(dir, "/"), "/") {
		if seg == "" {
			continue
		}
		cur += "/" + seg
		_ = conn.MakeDir(cur)
	}
}

func (d *ftpDriver) Write(ctx context.Context, key string, data []byte) error {
	return d.WriteStream(ctx, key, bytes.NewReader(data), int64(len(data)))
}

func (d *ftpDriver) WriteStream(ctx context.Context, key string, r io.Reader, _ int64) error {
	conn, err := d.connect()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Quit() }()
	abs := d.abs(key)
	d.mkdirAllRemote(conn, path.Dir(abs))
	return conn.Stor(abs, r)
}

func (d *ftpDriver) Read(ctx context.Context, key string) ([]byte, error) {
	rc, err := d.Reader(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return ReadAllLimited(rc, MaxWholeReadBytes)
}

func (d *ftpDriver) Reader(ctx context.Context, key string) (io.ReadCloser, error) {
	conn, err := d.connect()
	if err != nil {
		return nil, err
	}
	resp, err := conn.Retr(d.abs(key))
	if err != nil {
		_ = conn.Quit()
		return nil, d.notExistWrap(key, err)
	}
	// 包装：读端关闭/读完即断开控制连接
	return &ftpReadCloser{ReadCloser: resp, conn: conn}, nil
}

type ftpReadCloser struct {
	io.ReadCloser
	conn *ftp.ServerConn
}

func (f *ftpReadCloser) Close() error {
	err := f.ReadCloser.Close()
	_ = f.conn.Quit()
	return err
}

// Stat 经父目录 List 定位条目（FTP 无统一 MLST 保证）。
func (d *ftpDriver) Stat(ctx context.Context, key string) (*Metadata, error) {
	conn, err := d.connect()
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Quit() }()
	abs := d.abs(key)
	dir, name := path.Split(strings.TrimRight(abs, "/"))
	if name == "" {
		name = "/"
		dir = path.Dir(strings.TrimRight(abs, "/")) + "/"
	}
	entries, err := conn.List(strings.TrimSuffix(dir, "/") + "/")
	if err != nil {
		return nil, d.notExistWrap(key, err)
	}
	for _, e := range entries {
		if e.Name != name {
			continue
		}
		return &Metadata{
			Path:    key,
			Size:    int64(e.Size),
			IsDir:   e.Type == ftp.EntryTypeFolder,
			ModTime: e.Time,
		}, nil
	}
	return nil, fmt.Errorf("%w: ftp %s", os.ErrNotExist, abs)
}

func (d *ftpDriver) Delete(ctx context.Context, key string) error {
	conn, err := d.connect()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Quit() }()
	if err := conn.Delete(d.abs(key)); err != nil {
		return d.notExistWrap(key, err)
	}
	return nil
}

func (d *ftpDriver) RemoveAll(ctx context.Context, key string) error {
	if strings.Trim(key, "/") == "" {
		return errors.New("ftp: refusing RemoveAll with empty key")
	}
	conn, err := d.connect()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Quit() }()
	return d.removeAllRemote(conn, d.abs(key))
}

func (d *ftpDriver) removeAllRemote(conn *ftp.ServerConn, abs string) error {
	entries, err := conn.List(abs)
	if err != nil {
		// 目录不存在视为已删净
		if strings.Contains(err.Error(), "550") {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.Name == "." || e.Name == ".." {
			continue
		}
		child := abs + "/" + e.Name
		if e.Type == ftp.EntryTypeFolder {
			if err := d.removeAllRemote(conn, child); err != nil {
				return err
			}
		} else if err := conn.Delete(child); err != nil {
			return err
		}
	}
	return conn.RemoveDir(abs)
}

func (d *ftpDriver) MkdirAll(ctx context.Context, key string) error {
	conn, err := d.connect()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Quit() }()
	d.mkdirAllRemote(conn, d.abs(key))
	return nil
}

func (d *ftpDriver) List(ctx context.Context, key string) ([]*Metadata, error) {
	conn, err := d.connect()
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Quit() }()
	entries, err := conn.List(d.abs(key))
	if err != nil {
		return nil, d.notExistWrap(key, err)
	}
	out := make([]*Metadata, 0, len(entries))
	for _, e := range entries {
		if e.Name == "." || e.Name == ".." {
			continue
		}
		out = append(out, &Metadata{
			Path:    e.Name,
			Size:    int64(e.Size),
			IsDir:   e.Type == ftp.EntryTypeFolder,
			ModTime: e.Time,
		})
	}
	return out, nil
}

// Probe 认证级连通性验证：登录 + root 可达。
func (d *ftpDriver) Probe(ctx context.Context) error {
	conn, err := d.connect()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Quit() }()
	if _, err := conn.List(strings.TrimSuffix(d.root, "/") + "/"); err != nil && d.root != "" {
		return fmt.Errorf("ftp 根目录不可达: %w", err)
	}
	return nil
}

func (d *ftpDriver) notExistWrap(key string, err error) error {
	if err != nil && strings.Contains(err.Error(), "550") {
		return fmt.Errorf("%w: ftp %s", os.ErrNotExist, d.abs(key))
	}
	return err
}
