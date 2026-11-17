package presign

// redis_degrade_test.go —— 直传会话面的 **Redis 运行期故障注入**（L6 故障注入）。
//
// 与匿名面不同：presign 的会话元信息（presign:meta:<upload_id>）**只存 Redis**
// （无 DB 回源副本，DB 只有 blob 账本）。所以本域的降级口径不是「回落 DB」，
// 而是：
//
//   ① **快速失败 + 明确错误语义**：会话存储不可用 ⇒ 明确报「服务不可用」
//      （业务码 10006 ⇒ HTTP 503），而不是笼统的 500/30008「预签名生成失败」
//      ——前端要能分辨「服务暂时不可用（可回退普通上传通道）」与「你参数错了」。
//   ② **不伪装成「会话不存在」**：Redis 读失败绝不能报 ErrUploadNotFound
//      ——那会让客户端以为上传丢了、重新 init，把已传的对象变成孤儿
//      （与 app/share 的 ErrShareGone 纪律同款：不把基建故障装成「码不存在」）。
//   ③ **不留半成品**：Init 失败不得留下任何会话（Redis 侧无键、账本无行、
//      未建分享）。
//   ④ **不得偷偷回落进程内内存 KV**：内存 KV 是「redis.host 为空」的**单机**
//      模式语义（重启丢会话、不跨副本共享）；运行期 Redis 宕机时回落内存 KV
//      会让多副本部署下「A 副本建、B 副本完成」必然失败，且故障被静默掩盖。
//   ⑤ **恢复后自动切回 Redis 路径**：新会话的 meta 键**真的写进 miniredis**
//      （带 TTL），且宕机前已建的会话恢复后照常可读（故障没毒化服务）。
//
// 怎么让它变红（负对照，详见交付说明的实跑记录）：
//   - 把 Init 的 Redis 写失败原样上抛（不包成 typed error）⇒ ①红
//     （handler 落到 500/30008，前端无法区分）；
//   - 把 Complete/GetMeta 的 Redis 读失败改成「一律返回 ErrUploadNotFound」
//     ⇒ ②红（基建故障被装成「会话不存在」）；
//   - 把 Init 的失败路径改成「先写内存 KV 再报错」⇒ ③红（留下半成品会话）；
//   - 恢复后若客户端被永久降级闩锁（不再用 Redis）⇒ ⑤红（meta 键不出现）。

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pigeonbox/contracts/errcode"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newPresignFaultService 起 miniredis + 内存 sqlite + presign service。
//
// 与既有 newTestService 的差别只有一处：Redis 客户端用快速失败参数
// （MaxRetries=-1 / DialerRetries=1），否则故障期单条命令要等百毫秒级的
// 默认重试退避，判据会退化成时序竞态。
func newPresignFaultService(t *testing.T) (*Service, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{
		Addr:          mr.Addr(),
		MaxRetries:    -1,
		DialerRetries: 1,
		DialTimeout:   10 * time.Millisecond,
		ReadTimeout:   2 * time.Second,
	})
	t.Cleanup(func() { _ = rdb.Close() })

	svc := NewService(newPresignTestDB(t), rdb, "http://test.local", "test-signing-key")
	return svc, mr
}

// presignMetaKey 会话元信息键（与 presign.go 的 keyUploadMeta 同口径）。
func presignMetaKey(uploadID string) string { return "presign:meta:" + uploadID }

// TestPresign_RedisOutage_InitFailsWithoutHalfSession 宕机期 Init 失败且
// **不留半成品**。
func TestPresign_RedisOutage_InitFailsWithoutHalfSession(t *testing.T) {
	svc, mr := newPresignFaultService(t)
	share := &mockShareService{returnCode: "SHARE001"}
	svc.SetShareService(share)
	ctx := context.Background()

	mr.Close()

	res, err := svc.Init(ctx, InitMeta{FileName: "a.txt", FileSize: 100, ContentType: "text/plain"})
	require.Error(t, err, "会话存储不可用时 Init 必须失败（不得静默成功）")
	assert.Nil(t, res)

	// 不得留下任何会话痕迹：Redis 侧一个键都没有。
	assert.Empty(t, mr.Keys(), "Init 失败不得留下任何会话键（半成品）")

	// 不得偷偷回落进程内内存 KV（那是「单机模式」语义，运行期回落会破坏
	// 多副本一致性并掩盖故障）。
	_, isMem := svc.rdb.(memoryKV)
	assert.False(t, isMem, "运行期 Redis 宕机不得静默回落进程内内存 KV")

	// 不得建分享（会话都没建出来，更不该有分享）。
	share.mu.Lock()
	called := share.called
	share.mu.Unlock()
	assert.Zero(t, called, "Init 失败不得触发建分享")
}

// TestPresign_RedisOutage_InitErrorCarriesUnavailableCode 宕机期 Init 的错误
// 必须携带**业务码**「服务不可用」（10006 ⇒ HTTP 503），而不是笼统的
// 500/30008「预签名生成失败」。
//
// 为什么这是判据而不是口味问题：gen/handler/presign 的 Init 分支是
// 「typed error（实现 ErrCode()）⇒ NewTypedError 透传业务码；否则
// NewInternalError(30008) ⇒ HTTP 500」。⇒ Redis 宕机这种**可恢复**的基建
// 故障只有带上 10006，前端才能把它与「参数非法」区分开并回退普通上传通道
// （匿名面已有同类处理：gen/handler/share_anonymous 的 sanitizeInternalMsg
// 对 dial tcp/redis 特征回「服务暂时不可用，请稍后重试」）。
//
// 负对照：把 Init 里 Redis 写失败的错误原样上抛（不包 typed）⇒ 本用例转红
// （ErrCode() 不存在 ⇒ handler 落 500/30008）。
func TestPresign_RedisOutage_InitErrorCarriesUnavailableCode(t *testing.T) {
	svc, mr := newPresignFaultService(t)
	svc.SetShareService(&mockShareService{returnCode: "SHARE001"})
	ctx := context.Background()

	mr.Close()

	_, err := svc.Init(ctx, InitMeta{FileName: "a.txt", FileSize: 100, ContentType: "text/plain"})
	require.Error(t, err)

	var coder interface{ ErrCode() int }
	require.ErrorAs(t, err, &coder,
		"Redis 宕机必须报带业务码的 typed error（否则 handler 落 500/30008，前端无法回退）")
	assert.Equal(t, errcode.CodeUnavailable, coder.ErrCode(),
		"必须落在「服务不可用」（10006 ⇒ HTTP 503），而不是内部错误/预签名失败")
}

// TestPresign_RedisOutage_CompleteNotDisguisedAsNotFound 宕机期读会话**不得**
// 伪装成「会话不存在」。
//
// 伪装的后果很具体：客户端拿到 10004「upload not found」⇒ 以为这次上传丢了
// ⇒ 重新 init 一次传输 ⇒ 先前已传的 2.5GB 对象变成孤儿（真机 2026-11-03
// 同族事故的形状）。⇒ 基建故障必须以「故障」形态上抛。
//
// 负对照：把 GetMeta/Complete 的 Redis 读失败改成「一律 ErrUploadNotFound」
// ⇒ 本用例转红。
func TestPresign_RedisOutage_CompleteNotDisguisedAsNotFound(t *testing.T) {
	svc, mr := newPresignFaultService(t)
	svc.SetShareService(&mockShareService{returnCode: "SHARE001"})
	ctx := context.Background()

	// 健康期建会话。
	res, err := svc.Init(ctx, InitMeta{FileName: "a.txt", FileSize: 100, ContentType: "text/plain"})
	require.NoError(t, err)
	require.NotNil(t, res)

	mr.Close()

	_, err = svc.GetMeta(ctx, res.UploadID)
	require.Error(t, err, "Redis 宕机时读会话必须失败")
	assert.NotErrorIs(t, err, ErrUploadNotFound,
		"基建故障不得伪装成「会话不存在」（否则客户端会重新 init，已传对象成孤儿）")

	_, err = svc.Complete(ctx, res.UploadID, res.Token, "203.0.113.5")
	require.Error(t, err, "Redis 宕机时 Complete 必须失败")
	assert.NotErrorIs(t, err, ErrUploadNotFound,
		"基建故障不得伪装成「会话不存在」（否则客户端会重新 init，已传对象成孤儿）")
	assert.NotErrorIs(t, err, ErrUploadExpired, "基建故障不得伪装成「会话过期」")
	assert.NotErrorIs(t, err, ErrTokenInvalid, "基建故障不得伪装成「令牌非法」")
}

// TestPresign_RedisOutageThenRestart_RecoversRedisPath 恢复后自动切回
// Redis 路径：会话 meta 键**真的写进 miniredis**（带 TTL），且宕机前建的
// 会话恢复后照常可读（故障没毒化服务）。
func TestPresign_RedisOutageThenRestart_RecoversRedisPath(t *testing.T) {
	svc, mr := newPresignFaultService(t)
	share := &mockShareService{returnCode: "SHARE001"}
	svc.SetShareService(share)
	store := newFakeStorage()
	svc.SetStorage(store)
	ctx := context.Background()

	// ① 健康期：建会话 ⇒ meta 键进 miniredis（带 TTL）。
	before, err := svc.Init(ctx, InitMeta{FileName: "a.txt", FileSize: 100, ContentType: "text/plain"})
	require.NoError(t, err)
	require.True(t, mr.Exists(presignMetaKey(before.UploadID)), "健康期会话键应写进 miniredis")
	require.Greater(t, mr.TTL(presignMetaKey(before.UploadID)), time.Duration(0), "会话键必须带 TTL")

	// ② 宕机期：Init 失败、不留半成品。
	//    判据是「键集合没有变多」（miniredis 关停期间仍保留已有键，不能拿
	//    Keys() 为空来判——那是健康期会话的键）。
	keysBefore := mr.Keys()
	mr.Close()
	_, err = svc.Init(ctx, InitMeta{FileName: "b.txt", FileSize: 100, ContentType: "text/plain"})
	require.Error(t, err)
	assert.ElementsMatch(t, keysBefore, mr.Keys(), "宕机期失败不得留下任何新会话键（半成品）")

	// ③ 恢复期：同端口同客户端复活 ⇒ 自动切回 Redis 路径。
	require.NoError(t, mr.Restart())
	after, err := svc.Init(ctx, InitMeta{FileName: "c.txt", FileSize: 100, ContentType: "text/plain"})
	require.NoError(t, err, "恢复后 Init 必须成功")
	require.True(t, mr.Exists(presignMetaKey(after.UploadID)),
		"恢复后新会话键必须真的写进 miniredis（自动切回 Redis 路径）")
	require.Greater(t, mr.TTL(presignMetaKey(after.UploadID)), time.Duration(0), "恢复后会话键仍须带 TTL")

	// 宕机前建的会话恢复后照常可读（故障没毒化服务）。
	meta, err := svc.GetMeta(ctx, before.UploadID)
	require.NoError(t, err, "恢复后宕机前的会话应照常可读")
	assert.Equal(t, before.UploadID, meta.UploadID)
}
