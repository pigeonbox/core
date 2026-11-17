// minio_integration_test.go —— L3 集成层：MinIO（S3 兼容）真后端集成用例。
//
// 为什么要有这一层（此前 MinIO 只有「管理端测试连接」probe）：
//   - 管理端 probe 只验「桶在不在、凭据对不对」，**从不读写对象**；
//   - S3 驱动（minio-go）此前只有单测级覆盖（含离线预签名形态断言），
//     真后端的 PUT/GET/DELETE/覆盖/分片合并/预签名直传直下**从未跑过**；
//   - 2026-11-02 Azure Blob + HDFS 的真机联调实测出 4 个「单测全绿、真机不可用」
//     的缺陷 ⇒ 同族后端必须有常态化真后端用例。
//
// 真后端是谁：CI 里由 scripts/l3-s3-backend.sh 起 **RustFS**（MinIO 兼容的
// 真实 S3 服务端，GitHub Release 资产 sha256 钉版）。2026-11-18 实测：MinIO 官方
// 三条分发路径（Docker Hub / quay.io / dl.min.io）已不可匿名获取，继续钉 MinIO
// 容器会让这步恒红（恒红 = 没门禁）。**被测面不变**：驱动走 minio-go（S3 协议面）。
// 本机想用真 MinIO：把同样这四个 env 指到自己的 MinIO 端点即可跑同一批用例。
//
// 门控（env 四件套 + 强制开关）：
//
//	PB_TEST_MINIO_ENDPOINT     必填，例：127.0.0.1:9000（可带 http:// / https:// 前缀）
//	PB_TEST_MINIO_ACCESS_KEY   必填
//	PB_TEST_MINIO_SECRET_KEY   必填
//	PB_TEST_MINIO_BUCKET       必填
//	PB_REQUIRE_REMOTE_STORAGE  =1 时上面缺一即**判红**（不跳过）
//
// 纪律：「没扫到」不许看起来像「扫过且通过」——
//   - env 齐 ⇒ 真跑（建桶→写→读→覆盖→删→分片合并→列表→预签名直传直下）；
//   - env 缺 ⇒ 显式 SKIP（退出码 0，但日志里有 SKIP 行；CI 门禁会 grep 断言没跳过）；
//   - env 缺 + PB_REQUIRE_REMOTE_STORAGE=1 ⇒ t.Fatalf（红）。
package storage

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"
)

// MinIO 集成 env 键名。
const (
	minioEnvEndpoint  = "PB_TEST_MINIO_ENDPOINT"
	minioEnvAccessKey = "PB_TEST_MINIO_ACCESS_KEY"
	minioEnvSecretKey = "PB_TEST_MINIO_SECRET_KEY"
	minioEnvBucket    = "PB_TEST_MINIO_BUCKET"
	minioEnvRequire   = "PB_REQUIRE_REMOTE_STORAGE"
)

// minioTestConfig 读取 env 门控。
//
// 返回 ok=false ⇒ 该用例跳过（显式 SKIP）；PB_REQUIRE_REMOTE_STORAGE=1 时
// env 缺失直接判红——「没扫到」不等于「扫过且通过」。
func minioTestConfig(t *testing.T) (cfg *StorageConfig, ok bool) {
	t.Helper()
	endpoint := strings.TrimSpace(os.Getenv(minioEnvEndpoint))
	accessKey := strings.TrimSpace(os.Getenv(minioEnvAccessKey))
	secretKey := strings.TrimSpace(os.Getenv(minioEnvSecretKey))
	bucket := strings.TrimSpace(os.Getenv(minioEnvBucket))

	var missing []string
	if endpoint == "" {
		missing = append(missing, minioEnvEndpoint)
	}
	if accessKey == "" {
		missing = append(missing, minioEnvAccessKey)
	}
	if secretKey == "" {
		missing = append(missing, minioEnvSecretKey)
	}
	if bucket == "" {
		missing = append(missing, minioEnvBucket)
	}
	if len(missing) > 0 {
		if strings.TrimSpace(os.Getenv(minioEnvRequire)) == "1" {
			t.Fatalf("%s=1 但 MinIO 集成 env 缺失（%s）——不能把「没扫到」当通过",
				minioEnvRequire, strings.Join(missing, ","))
		}
		t.Skipf("SKIP: MinIO 集成 env 未配置（缺 %s）——本机无 MinIO 时跳过是正常的；CI 里带 env 真跑",
			strings.Join(missing, ","))
	}

	host, useSSL := minioNormalizeEndpoint(endpoint)
	return &StorageConfig{
		Type:      StorageTypeS3,
		DataPath:  t.TempDir(),
		BaseURL:   "http://minio.test",
		Endpoint:  host,
		AccessKey: accessKey,
		SecretKey: secretKey,
		Bucket:    bucket,
		Region:    "us-east-1",
		UseSSL:    useSSL,
		PathStyle: true, // MinIO/Ceph 自建场景：路径风格寻址
	}, true
}

// minioNormalizeEndpoint 归一 endpoint（允许带 http:// / https:// 前缀）。
func minioNormalizeEndpoint(raw string) (host string, useSSL bool) {
	raw = strings.TrimSpace(raw)
	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "https://"):
		return strings.TrimSuffix(raw[len("https://"):], "/"), true
	case strings.HasPrefix(lower, "http://"):
		return strings.TrimSuffix(raw[len("http://"):], "/"), false
	default:
		return strings.TrimSuffix(raw, "/"), false
	}
}

// ensureMinIOBucket 幂等建桶（桶已存在 ⇒ 不报错，重跑不炸）。
func ensureMinIOBucket(t *testing.T, cfg *StorageConfig) {
	t.Helper()
	host, useSSL := minioNormalizeEndpoint(cfg.Endpoint)
	client, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       useSSL,
		Region:       cfg.Region,
		BucketLookup: minio.BucketLookupPath,
	})
	require.NoError(t, err, "minio 客户端构造失败")

	ctx := context.Background()
	exists, err := client.BucketExists(ctx, cfg.Bucket)
	require.NoError(t, err, "BucketExists 失败（endpoint 不通或凭据不对？）")
	if !exists {
		require.NoError(t, client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{}), "建桶失败")
	}
	exists, err = client.BucketExists(ctx, cfg.Bucket)
	require.NoError(t, err)
	require.True(t, exists, "建桶后桶仍不存在")
}

// TestMinIOIntegrationLifecycle 真 MinIO 上的完整生命周期。
func TestMinIOIntegrationLifecycle(t *testing.T) {
	cfg, ok := minioTestConfig(t)
	if !ok {
		return
	}
	ensureMinIOBucket(t, cfg)

	svc, err := NewStorageServiceE(cfg)
	require.NoError(t, err, "MinIO 后端构造失败")
	require.Equal(t, StorageTypeS3, svc.EffectiveType(), "后端被降级成 local（测试会失去意义）")
	require.NoError(t, svc.InitError())

	exerciseRemoteStorageLifecycle(t, svc, "uploads/2026/11/18")
}

// TestMinIOIntegrationPresign 预签名**直传/直下**：签名 URL 必须真能被后端接受
// （签得出来 ≠ 后端认；这一段是 S3 独有的能力面）。
func TestMinIOIntegrationPresign(t *testing.T) {
	cfg, ok := minioTestConfig(t)
	if !ok {
		return
	}
	ensureMinIOBucket(t, cfg)

	svc, err := NewStorageServiceE(cfg)
	require.NoError(t, err)
	ctx := context.Background()
	const key = "uploads/2026/11/18/presigned.bin"

	// 直传：拿预签名 PUT URL 真发一次 HTTP PUT
	putURL, err := svc.PresignPutURL(ctx, key, 10*time.Minute)
	require.NoError(t, err, "预签名 PUT URL 生成失败")
	require.Contains(t, putURL, "X-Amz-Signature", "预签名 URL 必须带签名参数")

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, strings.NewReader("presigned payload"))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "预签名直传请求失败")
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode, "预签名直传必须被后端接受")

	// 对象确实落桶了（大小 + ETag）
	size, etag, err := svc.HeadObject(ctx, key)
	require.NoError(t, err)
	require.EqualValues(t, len("presigned payload"), size)
	require.NotEmpty(t, etag, "HeadObject 应返回 ETag")

	// 直下：预签名 GET 取回内容一致（并带出下载文件名）
	getURL, err := svc.PresignGetURL(ctx, key, 10*time.Minute, `attachment; filename="p.bin"`)
	require.NoError(t, err, "预签名 GET URL 生成失败")
	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, getURL, nil)
	require.NoError(t, err)
	getResp, err := http.DefaultClient.Do(getReq)
	require.NoError(t, err, "预签名直下请求失败")
	data, err := io.ReadAll(getResp.Body)
	require.NoError(t, err)
	require.NoError(t, getResp.Body.Close())
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	require.Equal(t, "presigned payload", string(data), "直下内容与直传内容不一致")

	// 清理
	require.NoError(t, svc.DeleteFile(ctx, key))
	require.False(t, svc.FileExists(ctx, key))
}

// TestMinIOIntegrationFaultSurfacing 故障注入下的错误语义：
// 指向**不可达的端点 / 错误凭据**时，对外 API 必须显式报错，
// 不许静默成功（静默成功 = 上线后数据写丢了都不知道）。
func TestMinIOIntegrationFaultSurfacing(t *testing.T) {
	cfg, ok := minioTestConfig(t)
	if !ok {
		return
	}
	ctx := context.Background()

	// ① 端点不可达 ⇒ 写入必须显式报错（任何 S3 实现都该如此）
	badEndpoint := *cfg
	badEndpoint.Endpoint = "127.0.0.1:1"
	svc, err := NewStorageServiceE(&badEndpoint)
	require.NoError(t, err, "配置本身合法（构造失败说明是配置校验问题，不是连通性问题）")
	require.Error(t, svc.SaveBytes(ctx, "uploads/2026/11/18/unreachable.txt", []byte("x")),
		"端点不可达时写入必须显式报错")

	// ② 错误凭据 ⇒ 写入必须被拒。
	//
	// ⚠️ 只有**强制鉴权**的后端才拒（真 MinIO 返 403）。部分 S3 兼容实现
	//（如 moto 测试替身）根本不校验签名 ⇒ 先探测后端是否强制鉴权，
	// 不强制时跳过该子断言（并在日志里说明），不把「替身不鉴权」误判成产品缺陷。
	badCred := *cfg
	badCred.SecretKey = "definitely-not-the-right-secret"
	if s3EnforcesAuth(t, &badCred) {
		svcBad, err := NewStorageServiceE(&badCred)
		require.NoError(t, err)
		require.Error(t, svcBad.SaveBytes(ctx, "uploads/2026/11/18/bad-cred.txt", []byte("x")),
			"错误凭据时写入必须被拒")
	} else {
		t.Log("该 S3 端点不校验签名（测试替身），跳过「错误凭据被拒」子断言")
	}
}

// s3EnforcesAuth 探测后端是否强制鉴权：拿错凭据探桶，
// 报错（403）或看不到桶 ⇒ 强制；仍能看见桶 ⇒ 不强制（测试替身）。
func s3EnforcesAuth(t *testing.T, cfg *StorageConfig) bool {
	t.Helper()
	host, useSSL := minioNormalizeEndpoint(cfg.Endpoint)
	client, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       useSSL,
		Region:       cfg.Region,
		BucketLookup: minio.BucketLookupPath,
	})
	require.NoError(t, err)

	exists, err := client.BucketExists(context.Background(), cfg.Bucket)
	if err != nil {
		return true
	}
	return !exists
}
