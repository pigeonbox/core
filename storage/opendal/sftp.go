package opendal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// sftpDriver 基于 pkg/sftp 的 SFTP 驱动（OpenSSH/Business NAS 通用）。
// 连接按操作建立（SSH 短连接，避免陈旧连接）。
// 认证：password 与 private_key（PEM 内容，env 注入友好）二选一，前者优先。
// host key 校验：默认宽松（InsecureIgnoreHostKey）；options.host_key 提供公钥
// 行（known_hosts 格式或 base64 类型+模数）时严格校验。
type sftpDriver struct {
	host       string // host:port
	username   string
	password   string
	privateKey string
	hostKey    string
	root       string
	timeout    time.Duration
}

// newSFTPDriver 构造 SFTP 驱动。
// options:
//
//	host        必填；host[:port]（port 缺省 22）
//	username    必填
//	password    与 private_key 二选一
//	private_key PEM 私钥内容（多行，env 注入友好）
//	host_key    可选；known_hosts 行或 base64 主机公钥（严格校验）
//	root        可选远端子目录
func newSFTPDriver(opts map[string]string) (*sftpDriver, error) {
	host := strings.TrimSpace(opts["host"])
	if host == "" {
		return nil, errors.New("sftp: host is required")
	}
	if !strings.Contains(host, ":") {
		host += ":22"
	}
	user := strings.TrimSpace(opts["username"])
	if user == "" {
		return nil, errors.New("sftp: username is required")
	}
	if opts["password"] == "" && opts["private_key"] == "" {
		return nil, errors.New("sftp: password 或 private_key 至少配置一项")
	}
	return &sftpDriver{
		host:       host,
		username:   user,
		password:   opts["password"],
		privateKey: opts["private_key"],
		hostKey:    opts["host_key"],
		root:       strings.Trim(opts["root"], "/"),
		timeout:    30 * time.Second,
	}, nil
}

// connect 建立 SSH+SFTP 客户端（调用方负责 Close 两者）。
func (d *sftpDriver) connect() (*sftp.Client, func(), error) {
	var auths []ssh.AuthMethod
	if d.password != "" {
		auths = append(auths, ssh.Password(d.password))
	}
	if d.privateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(d.privateKey))
		if err != nil {
			return nil, nil, fmt.Errorf("sftp 解析私钥失败: %w", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	hkc := ssh.InsecureIgnoreHostKey()
	if strings.TrimSpace(d.hostKey) != "" {
		if pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(d.hostKey))); err == nil {
			hkc = ssh.FixedHostKey(pk)
		}
	}
	cfg := &ssh.ClientConfig{
		User:            d.username,
		Auth:            auths,
		HostKeyCallback: hkc,
		Timeout:         d.timeout,
	}
	raw, err := netDialTimeout("tcp", d.host, d.timeout)
	if err != nil {
		return nil, nil, fmt.Errorf("sftp dial %s: %w", d.host, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(raw, d.host, cfg)
	if err != nil {
		_ = raw.Close()
		return nil, nil, fmt.Errorf("sftp handshake %s: %w", d.host, err)
	}
	cli, err := sftp.NewClient(ssh.NewClient(sshConn, chans, reqs))
	if err != nil {
		_ = sshConn.Close()
		return nil, nil, fmt.Errorf("sftp 子系统建立失败: %w", err)
	}
	cleanup := func() {
		_ = cli.Close()
	}
	return cli, cleanup, nil
}

// abs key → 远端绝对路径（root 挂载语义）
func (d *sftpDriver) abs(key string) string {
	return "/" + path.Join(d.root, key)
}

// netDialTimeout 独立包装便于测试注入。
func netDialTimeout(network, addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout(network, addr, timeout)
}

func (d *sftpDriver) notExistWrap(key string, err error) error {
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: sftp %s", os.ErrNotExist, d.abs(key))
	}
	return err
}

func (d *sftpDriver) Write(ctx context.Context, key string, data []byte) error {
	return d.WriteStream(ctx, key, strings.NewReader(string(data)), int64(len(data)))
}

func (d *sftpDriver) WriteStream(ctx context.Context, key string, r io.Reader, _ int64) error {
	cli, done, err := d.connect()
	if err != nil {
		return err
	}
	defer done()
	abs := d.abs(key)
	if dir := path.Dir(abs); dir != "" && dir != "/" {
		_ = cli.MkdirAll(dir)
	}
	f, err := cli.Create(abs)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(f, r)
	return err
}

func (d *sftpDriver) Read(ctx context.Context, key string) ([]byte, error) {
	cli, done, err := d.connect()
	if err != nil {
		return nil, err
	}
	defer done()
	f, err := cli.Open(d.abs(key))
	if err != nil {
		return nil, d.notExistWrap(key, err)
	}
	defer func() { _ = f.Close() }()
	return ReadAllLimited(f, MaxWholeReadBytes)
}

func (d *sftpDriver) Reader(ctx context.Context, key string) (io.ReadCloser, error) {
	cli, done, err := d.connect()
	if err != nil {
		return nil, err
	}
	f, err := cli.Open(d.abs(key))
	if err != nil {
		done()
		return nil, d.notExistWrap(key, err)
	}
	return &sftpReadCloser{File: f, done: done}, nil
}

type sftpReadCloser struct {
	*sftp.File
	done func()
}

func (s *sftpReadCloser) Close() error {
	err := s.File.Close()
	s.done()
	return err
}

func (d *sftpDriver) Stat(ctx context.Context, key string) (*Metadata, error) {
	cli, done, err := d.connect()
	if err != nil {
		return nil, err
	}
	defer done()
	info, err := cli.Stat(d.abs(key))
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

func (d *sftpDriver) Delete(ctx context.Context, key string) error {
	cli, done, err := d.connect()
	if err != nil {
		return err
	}
	defer done()
	if err := cli.Remove(d.abs(key)); err != nil {
		return d.notExistWrap(key, err)
	}
	return nil
}

func (d *sftpDriver) RemoveAll(ctx context.Context, key string) error {
	if strings.Trim(key, "/") == "" {
		return errors.New("sftp: refusing RemoveAll with empty key")
	}
	cli, done, err := d.connect()
	if err != nil {
		return err
	}
	defer done()
	abs := d.abs(key)
	if err := cli.RemoveAll(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (d *sftpDriver) MkdirAll(ctx context.Context, key string) error {
	cli, done, err := d.connect()
	if err != nil {
		return err
	}
	defer done()
	return cli.MkdirAll(d.abs(key))
}

func (d *sftpDriver) List(ctx context.Context, key string) ([]*Metadata, error) {
	cli, done, err := d.connect()
	if err != nil {
		return nil, err
	}
	defer done()
	fis, err := cli.ReadDir(d.abs(key))
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

// Probe 认证级连通性验证：登录 + root 可达。
func (d *sftpDriver) Probe(ctx context.Context) error {
	cli, done, err := d.connect()
	if err != nil {
		return err
	}
	defer done()
	if _, err := cli.Stat("/" + d.root); err != nil && d.root != "" {
		return fmt.Errorf("sftp 根目录不可达: %w", err)
	}
	return nil
}
