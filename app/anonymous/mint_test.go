package anonymous

import (
	"context"
	"testing"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMintForShare 文件分享铸造取件码（2026-10-07 对标"文件另有 6 位取件码"语义）。
// 铸造只写 KV 映射，不动 DB；永久分享/过期时间不铸造。
func TestMintForShare(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	expireAt := time.Now().Add(time.Hour)

	// Peek/Retrieve 的 DB 回源需要分享记录真实存在（生产语义：分享先于取件码）
	// 展示信息以 **DB** 为准（2026-10-25 删掉 Redis「展示信息快照」后）：
	// 原始名落 UUIDFileName、大小落 Size ⇒ fixture 按真实写入形态造。
	seed := func(code, fileName string, size int64, requireAuth bool) {
		require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
			Code:         code,
			FilePath:     "uploads/x/" + code + ".bin",
			UUIDFileName: fileName,
			Size:         size,
			ExpiredCount: -1, // -1=时间式无限次；Go 零值 0 语义是"次数耗尽即过期"
			RequireAuth:  requireAuth,
		}))
	}
	seed("shareabc", "demo.txt", 128, false)
	seed("sharepwd", "demo.txt", 128, true)
	seed("shareperm", "demo.txt", 128, false)

	t.Run("正常铸造并可解析", func(t *testing.T) {
		pickup, err := svc.MintForShare(ctx, "shareabc", "demo.txt", 128, false, &expireAt)
		require.NoError(t, err)
		require.Len(t, pickup, codeLength)

		// Peek 经映射解析回分享码（与 Retrieve 同一 lookup 路径）
		meta, fc, err := svc.Peek(ctx, pickup)
		require.NoError(t, err)
		assert.Equal(t, "shareabc", fc.Code)
		assert.Equal(t, "shareabc", meta.ShareCode)
		assert.Equal(t, "demo.txt", meta.FileName)
		assert.Equal(t, int64(128), meta.FileSize)
	})

	t.Run("永久分享已解锁铸造（真相源在 DB，详见 TestMintForShare_Permanent）", func(t *testing.T) {
		pickup, err := svc.MintForShare(ctx, "shareperm", "p.bin", 1, false, nil)
		require.NoError(t, err)
		assert.Len(t, pickup, codeLength, "落库持久化后永久分享同权铸造")
	})

	t.Run("过期时间已过不铸造", func(t *testing.T) {
		past := time.Now().Add(-time.Minute)
		pickup, err := svc.MintForShare(ctx, "shareold", "o.bin", 1, false, &past)
		require.NoError(t, err)
		assert.Empty(t, pickup)
	})

	t.Run("密码保护标记进展示信息", func(t *testing.T) {
		pickup, err := svc.MintForShare(ctx, "sharepwd", "s.bin", 1, true, &expireAt)
		require.NoError(t, err)
		require.Len(t, pickup, codeLength)
		_, fc, err := svc.Peek(ctx, pickup)
		require.NoError(t, err)
		assert.True(t, fc.RequireAuth)
	})
}

// TestMintForShare_Permanent 永久分享铸造（2026-10-08 落库持久化解锁）：
// 真相源在 DB pickup_code 列，KV 无法表达永久 TTL 的约束不再存在；
// 铸造不写 KV，取件经 lookupShareCode 的 DB 回退路径解析。
func TestMintForShare_Permanent(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "shareperm", FilePath: "uploads/x/shareperm.bin", ExpiredCount: -1,
	}))

	pickup, err := svc.MintForShare(ctx, "shareperm", "p.bin", 1, false, nil)
	require.NoError(t, err)
	require.Len(t, pickup, codeLength)

	// DB 列已落（真相源）
	fc, err := svc.fileCodeRepo.GetByCode(ctx, "shareperm")
	require.NoError(t, err)
	require.NotNil(t, fc.PickupCode)
	assert.Equal(t, pickup, *fc.PickupCode)

	// 过期时间已过不铸造（永久 nil 之外的防御分支不受影响）
	past := time.Now().Add(-time.Minute)
	empty, err := svc.MintForShare(ctx, "shareperm", "p.bin", 1, false, &past)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// TestPickupCodeSurvivesKVFlush 取件码重启存活（落库持久化的核心价值）：
// KV/memkv 全部清空（模拟单机内存模式重启）后，取件/Peek 经 DB 回退路径照常解析。
func TestPickupCodeSurvivesKVFlush(t *testing.T) {
	svc, mr, _ := newTestService(t)
	ctx := context.Background()
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code:         "shareabc",
		FilePath:     "uploads/x/shareabc.bin",
		UUIDFileName: "demo.txt", // DB 回退路径的展示名来自 DisplayName()
		ExpiredCount: -1,
	}))
	expireAt := time.Now().Add(time.Hour)
	pickup, err := svc.MintForShare(ctx, "shareabc", "demo.txt", 128, false, &expireAt)
	require.NoError(t, err)
	require.Len(t, pickup, codeLength)

	// 模拟重启：KV 全失（内存模式 memkv/Redis 均适用）
	mr.FlushAll()

	meta, fc, err := svc.Peek(ctx, pickup)
	require.NoError(t, err)
	assert.Equal(t, "shareabc", fc.Code)
	assert.Equal(t, "shareabc", meta.ShareCode)
	assert.Equal(t, "demo.txt", meta.FileName, "DB 回退路径经 enrichMetaFromDB 补齐展示信息")
}

// TestCancelClearsPickupColumn Cancel 作废：KV 映射与 DB 列同步清除。
func TestCancelClearsPickupColumn(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "shareabc", FilePath: "uploads/x/shareabc.bin", ExpiredCount: -1,
	}))
	expireAt := time.Now().Add(time.Hour)
	pickup, err := svc.MintForShare(ctx, "shareabc", "demo.txt", 128, false, &expireAt)
	require.NoError(t, err)

	require.NoError(t, svc.Cancel(ctx, pickup))

	fc, err := svc.fileCodeRepo.GetByCode(ctx, "shareabc")
	require.NoError(t, err)
	assert.Nil(t, fc.PickupCode, "DB 列应已清空")
	_, _, err = svc.Peek(ctx, pickup)
	assert.Error(t, err, "KV+DB 均清后取件应未命中")
}
