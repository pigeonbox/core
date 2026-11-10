package opendal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// s3Driver 基于 minio-go 的 S3/兼容存储驱动（AWS S3/MinIO/Ceph/OSS/COS/B2/...）。
// key 为相对路径，bucket 由驱动持有。
type s3Driver struct {
	client *minio.Client
	bucket string
}

// newS3Driver 构造 S3 驱动。
// options:
//
//	endpoint   必填；兼容带/不带 scheme（https:// 前缀自动启用 TLS）
//	access_key / secret_key  必填
//	bucket     必填（Operator.New 会把 Config.Root 兜底进来）
//	region     可选
//	use_ssl    可选 "true"/"false"；endpoint 带 scheme 时以 scheme 为准
//	path_style 可选 "true" 走路径风格寻址（MinIO/Ceph 自建场景常用）
func newS3Driver(opts map[string]string) (*s3Driver, error) {
	endpoint := strings.TrimSpace(opts["endpoint"])
	if endpoint == "" {
		return nil, errors.New("s3: endpoint is required")
	}
	accessKey, secretKey := strings.TrimSpace(opts["access_key"]), strings.TrimSpace(opts["secret_key"])
	if accessKey == "" || secretKey == "" {
		return nil, errors.New("s3: access_key/secret_key are required")
	}
	bucket := strings.TrimSpace(opts["bucket"])
	if bucket == "" {
		return nil, errors.New("s3: bucket is required")
	}

	useSSL := opts["use_ssl"] == "true"
	lower := strings.ToLower(endpoint)
	switch {
	case strings.HasPrefix(lower, "https://"):
		useSSL, endpoint = true, endpoint[len("https://"):]
	case strings.HasPrefix(lower, "http://"):
		useSSL, endpoint = false, endpoint[len("http://"):]
	}
	endpoint = strings.TrimSuffix(strings.TrimSpace(endpoint), "/")
	if u, err := url.Parse("scheme://" + endpoint); err != nil || u.Host == "" {
		return nil, fmt.Errorf("s3: invalid endpoint %q", opts["endpoint"])
	}

	lookup := minio.BucketLookupAuto
	switch {
	case opts["force_virtual_host"] == "true":
		// 云厂商预设桶均为 virtual-host 风格。minio-go 的 Auto 对自定义端点
		// 退化为 path-style，部分桶/地域（如 COS ap-beijing 新桶）直接
		// PathStyleDomainForbidden——云厂商分支必须显式强制 virtual-host。
		lookup = minio.BucketLookupDNS
	case opts["path_style"] == "true":
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure:       useSSL,
		Region:       opts["region"],
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("s3: new client: %w", err)
	}
	return &s3Driver{client: client, bucket: bucket}, nil
}

// wrapErr 把 S3 的 NoSuchKey/404 归一为 os.ErrNotExist，调用方用 errors.Is 判断。
func (d *s3Driver) wrapErr(key string, err error) error {
	if err == nil {
		return nil
	}
	resp := minio.ToErrorResponse(err)
	if resp.Code == "NoSuchKey" || resp.Code == "NotFound" || resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: s3://%s/%s", os.ErrNotExist, d.bucket, key)
	}
	return err
}

func (d *s3Driver) Write(ctx context.Context, key string, data []byte) error {
	_, err := d.client.PutObject(ctx, d.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return d.wrapErr(key, err)
}

func (d *s3Driver) WriteStream(ctx context.Context, key string, r io.Reader, size int64) error {
	_, err := d.client.PutObject(ctx, d.bucket, key, r, size,
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return d.wrapErr(key, err)
}

func (d *s3Driver) Read(ctx context.Context, key string) ([]byte, error) {
	obj, err := d.client.GetObject(ctx, d.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, d.wrapErr(key, err)
	}
	defer func() { _ = obj.Close() }()
	data, err := ReadAllLimited(obj, MaxWholeReadBytes)
	if err != nil {
		return nil, d.wrapErr(key, err)
	}
	return data, nil
}

func (d *s3Driver) Reader(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := d.client.GetObject(ctx, d.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, d.wrapErr(key, err)
	}
	return obj, nil
}

// ReadRange 按 [start, start+length) 区间读取（S3 原生 Range GET，断点续传用）。
// length<=0 表示从 start 读到末尾。
func (d *s3Driver) ReadRange(ctx context.Context, key string, start, length int64) (io.ReadCloser, error) {
	opts := minio.GetObjectOptions{}
	if length > 0 {
		if err := opts.SetRange(start, start+length-1); err != nil {
			return nil, d.wrapErr(key, err)
		}
	} else if start > 0 {
		if err := opts.SetRange(start, 0); err != nil {
			return nil, d.wrapErr(key, err)
		}
	}
	obj, err := d.client.GetObject(ctx, d.bucket, key, opts)
	if err != nil {
		return nil, d.wrapErr(key, err)
	}
	return obj, nil
}

func (d *s3Driver) Stat(ctx context.Context, key string) (*Metadata, error) {
	info, err := d.client.StatObject(ctx, d.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return nil, d.wrapErr(key, err)
	}
	return &Metadata{
		Path:    key,
		Size:    info.Size,
		ModTime: info.LastModified,
		ETag:    info.ETag,
	}, nil
}

func (d *s3Driver) Delete(ctx context.Context, key string) error {
	return d.client.RemoveObject(ctx, d.bucket, key, minio.RemoveObjectOptions{})
}

// RemoveAll 删除 key 前缀下所有对象。
// 对象存储没有真正的目录对象：列出 key/ 前缀逐个删除，再尽力删掉
// 可能单独存在的 key 本体（不存在时 S3 幂等成功，错误忽略）。
func (d *s3Driver) RemoveAll(ctx context.Context, key string) error {
	prefix := strings.Trim(key, "/")
	if prefix == "" {
		return errors.New("s3: refusing RemoveAll with empty prefix")
	}
	objCh := d.client.ListObjects(ctx, d.bucket,
		minio.ListObjectsOptions{Prefix: prefix + "/", Recursive: true})
	for obj := range objCh {
		if obj.Err != nil {
			return obj.Err
		}
		if err := d.client.RemoveObject(ctx, d.bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
			return err
		}
	}
	_ = d.client.RemoveObject(ctx, d.bucket, prefix, minio.RemoveObjectOptions{})
	return nil
}

func (d *s3Driver) List(ctx context.Context, key string) ([]*Metadata, error) {
	prefix := strings.Trim(key, "/")
	if prefix != "" {
		prefix += "/"
	}
	objCh := d.client.ListObjects(ctx, d.bucket,
		minio.ListObjectsOptions{Prefix: prefix, Recursive: false})
	var out []*Metadata
	for obj := range objCh {
		if obj.Err != nil {
			return nil, obj.Err
		}
		isDir := strings.HasSuffix(obj.Key, "/")
		rel := strings.TrimPrefix(obj.Key, prefix)
		out = append(out, &Metadata{
			Path:    strings.TrimSuffix(rel, "/"),
			Size:    obj.Size,
			IsDir:   isDir,
			ModTime: obj.LastModified,
			ETag:    obj.ETag,
		})
	}
	return out, nil
}

func (d *s3Driver) Copy(ctx context.Context, src, dst string) error {
	_, err := d.client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: d.bucket, Object: dst},
		minio.CopySrcOptions{Bucket: d.bucket, Object: src})
	return d.wrapErr(dst, err)
}

func (d *s3Driver) Rename(ctx context.Context, src, dst string) error {
	if err := d.Copy(ctx, src, dst); err != nil {
		return err
	}
	return d.Delete(ctx, src)
}

// Presign 离线签名生成真实 S3 预签名 URL（无需网络），客户端可直传/直下。
// opts 透传为签名 query 参数（GET 常用 response-content-disposition 指定
// 下载呈现名——直下 302 的最终响应头由对象存储回给客户端）。
func (d *s3Driver) Presign(ctx context.Context, method, key string, expire time.Duration, opts map[string]string) (*PresignedResult, error) {
	switch strings.ToUpper(method) {
	case http.MethodPut:
		u, err := d.client.PresignedPutObject(ctx, d.bucket, key, expire)
		if err != nil {
			return nil, err
		}
		return &PresignedResult{URL: u.String(), Method: http.MethodPut}, nil
	case http.MethodGet:
		params := url.Values{}
		for k, v := range opts {
			params.Set(k, v)
		}
		u, err := d.client.PresignedGetObject(ctx, d.bucket, key, expire, params)
		if err != nil {
			return nil, err
		}
		return &PresignedResult{URL: u.String(), Method: http.MethodGet}, nil
	default:
		return nil, fmt.Errorf("s3: presign unsupported method %q", method)
	}
}

// BucketExists 认证级连通性验证（Probe 用）：能校验凭据与桶是否存在。
func (d *s3Driver) BucketExists(ctx context.Context) (bool, error) {
	return d.client.BucketExists(ctx, d.bucket)
}

// Probe 认证级连通性验证：凭据有效且桶存在才算通过。
func (d *s3Driver) Probe(ctx context.Context) error {
	exists, err := d.BucketExists(ctx)
	if err != nil {
		return fmt.Errorf("s3 连接失败: %w", err)
	}
	if !exists {
		return fmt.Errorf("s3 桶 %q 不存在或无权访问", d.bucket)
	}
	return nil
}
