// remote_integration_common_test.go —— L3 集成层：远端存储后端的**统一行为契约**。
//
// 本文件只放「跨后端共用」的那部分断言（写入→读回→覆盖→存在→大小→删除→
// 分片合并→前缀列表），由两个文件各跑一遍：
//
//   - webdav_integration_test.go：进程内真 WebDAV 服务（x/net/webdav）
//   - minio_integration_test.go：真 MinIO 容器（env 门控，CI 跑）
//
// 为什么抽成共用契约：两个后端此前**都只有单测级覆盖**（WebDAV 驱动单测 +
// 管理端 probe），从没在真后端上验过同一份契约；契约一旦漂移（例如某后端把
// 「覆盖写」悄悄变成追加、或分片合并顺序反了），单测层面完全看不出来。
//
// ⚠️ 断言落在**行为**上（内容逐字一致 / 大小一致 / 错误语义正确），
//
//	不是「函数没报错」。
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// exerciseRemoteStorageLifecycle 跑一遍远端存储的统一行为契约。
//
// prefix 是本次跑批的 key 前缀（不同后端/不同跑批互不干扰）。
// 调用方负责构造 svc（真后端，且已确认不是降级 local）。
func exerciseRemoteStorageLifecycle(t *testing.T, svc *StorageService, prefix string) {
	t.Helper()
	ctx := context.Background()

	// ── ① 写入 → 读回（内容逐字一致，不是「没报错」）──
	payload := []byte("pigeonbox 远端存储集成 payload｜中文｜" + prefix)
	key := prefix + "/plain.txt"
	require.NoError(t, svc.SaveBytes(ctx, key, payload), "SaveBytes 写入应成功")
	got, err := svc.GetFile(ctx, key)
	require.NoError(t, err, "GetFile 读回应成功")
	require.Equal(t, payload, got, "读回内容与写入内容不一致")

	// ── ② 存在性 / 大小（stat）三者一致 ──
	require.True(t, svc.FileExists(ctx, key), "写入后应存在")
	size, err := svc.GetFileSize(ctx, key)
	require.NoError(t, err)
	require.EqualValues(t, len(payload), size, "GetFileSize 与写入长度不符")
	st, err := svc.StatFile(ctx, key)
	require.NoError(t, err)
	require.EqualValues(t, len(payload), st, "StatFile 与写入长度不符")

	// ── ③ 流式读（GetFileReader）：内容与长度都对 ──
	rc, n, err := svc.GetFileReader(ctx, key)
	require.NoError(t, err)
	require.EqualValues(t, len(payload), n, "GetFileReader 返回的长度不符")
	streamed, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, payload, streamed, "流式读回内容与写入内容不一致")

	// ── ④ 覆盖写：同 key 再写 ⇒ 内容被**替换**（不许追加/残留旧内容）──
	require.NoError(t, svc.SaveBytes(ctx, key, []byte("v2")), "覆盖写应成功")
	got2, err := svc.GetFile(ctx, key)
	require.NoError(t, err)
	require.Equal(t, "v2", string(got2), "覆盖写后应只剩新内容")
	size2, err := svc.GetFileSize(ctx, key)
	require.NoError(t, err)
	require.EqualValues(t, 2, size2, "覆盖写后大小应是新内容长度")

	// ── ⑤ multipart 上传（SaveFile）：文件哈希与内容一致 ──
	content := []byte("multipart payload")
	fh := newTestFileHeader(t, "b.bin", content)
	res, err := svc.SaveFile(ctx, fh, prefix+"/multipart.bin")
	require.NoError(t, err)
	require.True(t, res.Success)
	require.EqualValues(t, len(content), res.FileSize, "SaveFile 返回的大小不符")
	sum := sha256.Sum256(content)
	require.Equal(t, hex.EncodeToString(sum[:]), res.FileHash, "SaveFile 返回的 SHA-256 不符")
	body, err := svc.GetFile(ctx, prefix+"/multipart.bin")
	require.NoError(t, err)
	require.Equal(t, content, body, "multipart 上传后读回内容不一致")

	// ── ⑥ 删除：删完不存在；再读必须是 os.ErrNotExist 语义；
	//      重复删除必须报错（不许静默成功）──
	require.NoError(t, svc.DeleteFile(ctx, key), "首次删除应成功")
	require.False(t, svc.FileExists(ctx, key), "删除后不应存在")
	_, err = svc.GetFile(ctx, key)
	require.Error(t, err, "删除后再读必须报错")
	require.ErrorIs(t, err, os.ErrNotExist, "删除后再读必须是 os.ErrNotExist 语义")
	require.Error(t, svc.DeleteFile(ctx, key), "重复删除必须报错（不许静默成功）")

	// ── ⑦ 分片保存 + 合并（chunk 语义）：按分片序号拼接 ──
	uploadID := "upl-" + strings.ReplaceAll(prefix, "/", "-")
	parts := [][]byte{[]byte("chunk-0|"), []byte("chunk-1|"), []byte("chunk-2|")}
	for i, p := range parts {
		require.NoError(t, svc.SaveChunk(ctx, uploadID, i, p), "SaveChunk 应成功")
	}
	mergedKey := prefix + "/merged.bin"
	require.NoError(t, svc.MergeChunks(ctx, uploadID, len(parts), mergedKey), "MergeChunks 应成功")
	merged, err := svc.GetFile(ctx, mergedKey)
	require.NoError(t, err)
	require.Equal(t, "chunk-0|chunk-1|chunk-2|", string(merged), "合并产物必须按分片序号拼接")
	mergedSize, err := svc.GetFileSize(ctx, mergedKey)
	require.NoError(t, err)
	require.EqualValues(t, len("chunk-0|chunk-1|chunk-2|"), mergedSize, "合并产物大小不符")

	// ── ⑧ 合并后分片被清理（MergeChunks 内是异步清理 ⇒ 轮询等待）──
	require.Eventually(t, func() bool {
		return !svc.FileExists(ctx, "chunks/"+uploadID+"/chunk_0")
	}, 10*time.Second, 50*time.Millisecond, "合并后分片目录应被清理，不许留垃圾")

	// ── ⑨ 前缀列表：列表里能看到刚写的对象 ──
	objs, err := svc.ListRemoteObjects(ctx, prefix)
	require.NoError(t, err)
	require.True(t, hasRemoteKey(objs, prefix+"/multipart.bin"),
		"前缀列表应包含 multipart.bin，实际 %v", remoteKeys(objs))
	require.True(t, hasRemoteKey(objs, mergedKey),
		"前缀列表应包含 merged.bin，实际 %v", remoteKeys(objs))
}

// hasRemoteKey 判断列表结果里是否含某个 key。
//
// 兼容两种驱动口径：S3 驱动返回「相对 prefix 的路径」，WebDAV 驱动返回
// 「目录项名」（fi.Name()）——两种都算命中（未来任一驱动改成返回全路径也仍命中）。
func hasRemoteKey(objs []RemoteObjectInfo, key string) bool {
	base := path.Base(key)
	for _, o := range objs {
		if o.Key == key || o.Key == base || strings.HasSuffix(o.Key, "/"+base) {
			return true
		}
	}
	return false
}

// remoteKeys 列表 key（失败信息里用）。
func remoteKeys(objs []RemoteObjectInfo) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.Key)
	}
	return out
}
