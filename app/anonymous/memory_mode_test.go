package anonymous

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newMemoryTestService 构造内存模式（rdb=nil）+ sqlite 内存库的 anonymous service。
func newMemoryTestService(t *testing.T) *Service {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}))
	// glebarez/sqlite 的 :memory: 每条连接是独立库；取件码落库后并发铸造会多连接
	// 并取，必须钉死单连接（同 share/service_test.go newTestDB 的既有教训）
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return NewService(nil, g)
}

// TestMemoryMode_IsMemoryKV 构造函数 nil 兜底应进入内存模式。
func TestMemoryMode_IsMemoryKV(t *testing.T) {
	svc := newMemoryTestService(t)
	_, ok := svc.rdb.(memoryKV)
	assert.True(t, ok, "rdb=nil 应兜底为内存 KV")
}

// TestMemoryMode_EndToEnd 内存模式端到端：建分享 → Peek → Retrieve（扣次数）→ Cancel 后 404。
func TestMemoryMode_EndToEnd(t *testing.T) {
	svc := newMemoryTestService(t)
	ctx := context.Background()

	expire := time.Now().Add(time.Hour)
	code, err := svc.CreateAnonymousShare(ctx, AnonymousShareParams{
		FilePath:    "uploads/2026/10/06/demo.bin",
		FileName:    "demo.bin",
		FileSize:    123,
		ContentType: "application/octet-stream",
		ExpireAt:    &expire,
	})
	require.NoError(t, err)
	assert.Len(t, code, codeLength)

	meta, fc, err := svc.Peek(ctx, code)
	require.NoError(t, err)
	assert.Equal(t, "demo.bin", meta.FileName)
	require.NotNil(t, fc)

	meta, err = svc.Retrieve(ctx, code, "", "")
	require.NoError(t, err)
	assert.Equal(t, int64(123), meta.FileSize)

	require.NoError(t, svc.Cancel(ctx, code))
	_, _, err = svc.Peek(ctx, code)
	assert.ErrorIs(t, err, ErrCodeNotFound)
}

// TestMemoryMode_GenerateCode_Concurrent 内存模式 NX 并发生成不冲突。
func TestMemoryMode_GenerateCode_Concurrent(t *testing.T) {
	svc := newMemoryTestService(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)
	// 取件码落库为真相源（2026-10-08）：铸造要求分享行已存在（生产链路
	// CreateAnonymousShare 先建行后铸造，与此一致）
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "SHARE_C", FilePath: "uploads/x/SHARE_C.bin", ExpiredCount: -1,
	}))

	var wg sync.WaitGroup
	codes := make(chan string, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := svc.GenerateCode(ctx, CodeMeta{ShareCode: "SHARE_C"}, exp)
			if err == nil {
				codes <- c
			}
		}()
	}
	wg.Wait()
	close(codes)

	seen := map[string]bool{}
	for c := range codes {
		assert.False(t, seen[c], "重复码: %s", c)
		seen[c] = true
	}
	assert.Len(t, seen, 50)
}

// TestMemoryMode_TextShareViaShareCode 文本分享无取件码，凭 8 位分享码回源 DB 取件。
func TestMemoryMode_TextShareViaShareCode(t *testing.T) {
	svc := newMemoryTestService(t)
	ctx := context.Background()
	expire := time.Now().Add(time.Hour)
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code:         "abcd1234",
		FilePath:     "", // 文本分享：FilePath 空
		UUIDFileName: "note.txt",
		Text:         "hello",
		ExpiredAt:    &expire,
		ExpiredCount: -1,
		UploadType:   "text",
	}))

	shareCode, err := svc.lookupShareCode(ctx, "abcd1234")
	require.NoError(t, err)
	assert.Equal(t, "abcd1234", shareCode)
}

// TestLookupShareCode_NegativeCache 映射与 DB 均无此码 → 放负缓存标记；
// 标记存在期间重复查询直接 404（不再打库）。
func TestLookupShareCode_NegativeCache(t *testing.T) {
	svc := newMemoryTestService(t)
	ctx := context.Background()
	code := "zzzz99" // 6 位、字符表内、DB 无此记录（lookup 内部规范化为大写）

	_, err := svc.lookupShareCode(ctx, code)
	assert.ErrorIs(t, err, ErrCodeNotFound)

	mkv := svc.rdb.(memoryKV)
	_, marked := mkv.Store.Get(fmt.Sprintf(keyPickupCodeNeg, strings.ToUpper(code)))
	assert.True(t, marked, "DB 未命中后应放负缓存标记")

	_, err = svc.lookupShareCode(ctx, code)
	assert.ErrorIs(t, err, ErrCodeNotFound)
}

// TestLookupShareCode_DBBackfill 回源命中后回填映射缓存；DB 记录消失后
// 短 TTL 内仍可凭回填缓存解析（真实状态以 DB 为准的边界外由过期兜底）。
func TestLookupShareCode_DBBackfill(t *testing.T) {
	svc := newMemoryTestService(t)
	ctx := context.Background()
	expire := time.Now().Add(time.Hour)
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code:         "abcdefgh",
		FilePath:     "uploads/2026/10/06/backfill.bin",
		ExpiredAt:    &expire,
		ExpiredCount: -1,
	}))

	// 首查：KV 未命中 → DB 命中 → 回填
	shareCode, err := svc.lookupShareCode(ctx, "abcdefgh")
	require.NoError(t, err)
	assert.Equal(t, "abcdefgh", shareCode)

	mkv := svc.rdb.(memoryKV)
	cached, ok := mkv.Store.Get(fmt.Sprintf(keyPickupCodeMapping, "abcdefgh"))
	require.True(t, ok, "回源命中应回填映射缓存")
	assert.Equal(t, "abcdefgh", cached)

	// 删 DB 记录，二查走回填缓存仍解析成功（缓存仅加速解析）
	fc, err := svc.fileCodeRepo.GetByCode(ctx, "abcdefgh")
	require.NoError(t, err)
	require.NotNil(t, fc)
	require.NoError(t, svc.fileCodeRepo.Delete(ctx, fc.ID))
	shareCode, err = svc.lookupShareCode(ctx, "abcdefgh")
	require.NoError(t, err)
	assert.Equal(t, "abcdefgh", shareCode)
}

// TestMemoryMode_RedisAndMemoryParity 同一业务流在 miniredis 与内存模式下的
// 结果一致性（防双实现漂移）。
func TestMemoryMode_RedisAndMemoryParity(t *testing.T) {
	ctx := context.Background()

	runFlow := func(t *testing.T, svc *Service) (pickupCode string, retrieved bool, notFoundErr error) {
		expire := time.Now().Add(time.Hour)
		code, err := svc.CreateAnonymousShare(ctx, AnonymousShareParams{
			FilePath:    "uploads/2026/10/06/parity.bin",
			FileName:    "parity.bin",
			FileSize:    7,
			ContentType: "application/octet-stream",
			ExpireAt:    &expire,
		})
		if err != nil {
			return "", false, err
		}
		if _, err := svc.Retrieve(ctx, code, "", ""); err != nil {
			return code, false, err
		}
		_, err = svc.Retrieve(ctx, "ZZZZ99", "", "")
		if !errors.Is(err, ErrCodeNotFound) {
			return code, true, fmt.Errorf("期望 ErrCodeNotFound, got %w", err)
		}
		return code, true, nil
	}

	memSvc := newMemoryTestService(t)
	_, _, memErr := runFlow(t, memSvc)
	require.NoError(t, memErr)

	redisSvc, _, _ := newTestService(t) // miniredis 形态（真实 Redis 协议路径）
	_, _, rdErr := runFlow(t, redisSvc)
	require.NoError(t, rdErr)
}
