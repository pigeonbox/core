package anonymous

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newTestService 构造注入 miniredis + sqlite 内存库的 anonymous service。
func newTestService(t *testing.T) (*Service, *miniredis.Miniredis, *gorm.DB) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}))
	// glebarez/sqlite 的 :memory: 每条连接是独立库；取件码落库后并发铸造会多连接
	// 并取，必须钉死单连接（同 share/service_test.go newTestDB 的既有教训）
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	return NewService(rdb, g), mr, g
}

// TestCodeAlphabet 验证字符表（去掉易混淆字符 0/O/1/I/L）
func TestCodeAlphabet(t *testing.T) {
	for _, ch := range codeAlphabet {
		assert.NotContains(t, "0O1IL", string(ch))
	}
}

// TestGenerateCode_1000Uniqueness 生成 1000 个码不重复
func TestGenerateCode_1000Uniqueness(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		c := randomCode()
		assert.Len(t, c, codeLength)
		assert.False(t, seen[c], "重复码: %s", c)
		seen[c] = true
	}
	_ = svc
	_ = ctx
}

// TestGenerateCode_Concurrent 并发生成不冲突（SETNX 保证）
func TestGenerateCode_Concurrent(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "SHARE_C", FilePath: "uploads/x/SHARE_C.bin", ExpiredCount: -1}))

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
		assert.False(t, seen[c], "并发冲突: %s", c)
		seen[c] = true
	}
}

// TestRetrieve_HappyPath 正常取件
func TestRetrieve_HappyPath(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "SHARE1", FilePath: "uploads/x/SHARE1.bin", ExpiredCount: 3}))

	code, err := svc.GenerateCode(ctx, CodeMeta{
		ShareCode: "SHARE1", FileName: "f.txt", FileSize: 100, ContentType: "text/plain",
	}, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Len(t, code, 6)

	meta, err := svc.Retrieve(ctx, code, "", "")
	require.NoError(t, err)
	assert.Equal(t, "SHARE1", meta.ShareCode)
	assert.Equal(t, "f.txt", meta.FileName)
	assert.Equal(t, int64(100), meta.FileSize)

	// DB 次数已扣减
	fc, _ := svc.fileCodeRepo.GetByCode(ctx, "SHARE1")
	assert.Equal(t, 2, fc.ExpiredCount)
	assert.Equal(t, 1, fc.UsedCount)
}

// TestRetrieve_NotFound 取件码不存在
func TestRetrieve_NotFound(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, err := svc.Retrieve(context.Background(), "NOEXIST", "", "")
	assert.ErrorIs(t, err, ErrCodeNotFound)
}

// TestRetrieve_Expired 时间过期
func TestRetrieve_Expired(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "SHARE_EXP", ExpiredAt: &past, ExpiredCount: -1}))
	code, err := svc.GenerateCode(ctx, CodeMeta{ShareCode: "SHARE_EXP"}, time.Now().Add(time.Hour))
	require.NoError(t, err)

	_, err = svc.Retrieve(ctx, code, "", "")
	assert.ErrorIs(t, err, ErrCodeExpired)
}

// TestRetrieve_Exhausted 次数耗尽
func TestRetrieve_Exhausted(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "SHARE_EXH", FilePath: "uploads/x/SHARE_EXH.bin", ExpiredCount: 1}))
	code, err := svc.GenerateCode(ctx, CodeMeta{ShareCode: "SHARE_EXH"}, time.Now().Add(time.Hour))
	require.NoError(t, err)

	// 第一次成功（1→0）
	_, err = svc.Retrieve(ctx, code, "", "")
	require.NoError(t, err)
	// 第二次：ExpiredCount=0 → IsExpired=true
	_, err = svc.Retrieve(ctx, code, "", "")
	assert.ErrorIs(t, err, ErrCodeExpired)
}

// TestRetrieve_PasswordWrong 密码错误
func TestRetrieve_PasswordWrong(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	hash, err := utils.HashPassword("right")
	require.NoError(t, err)
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "SHARE_PW", FilePath: "uploads/x/SHARE_PW.bin", ExpiredCount: -1, RequireAuth: true, PasswordHash: hash}))
	code, err := svc.GenerateCode(ctx, CodeMeta{ShareCode: "SHARE_PW", RequireAuth: true}, time.Now().Add(time.Hour))
	require.NoError(t, err)

	// 错误密码
	_, err = svc.Retrieve(ctx, code, "wrong", "")
	assert.ErrorIs(t, err, ErrPasswordWrong)

	// 正确密码
	meta, err := svc.Retrieve(ctx, code, "right", "")
	require.NoError(t, err)
	assert.Equal(t, "SHARE_PW", meta.ShareCode)
}

// TestRetrieve_NoPasswordButRequired 需要密码但未传
func TestRetrieve_NoPasswordButRequired(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	hash, _ := utils.HashPassword("secret")
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "SHARE_NP", FilePath: "uploads/x/SHARE_NP.bin", ExpiredCount: -1, RequireAuth: true, PasswordHash: hash}))
	code, _ := svc.GenerateCode(ctx, CodeMeta{ShareCode: "SHARE_NP", RequireAuth: true}, time.Now().Add(time.Hour))

	// 空密码 → CheckPassword("", "") 对 bcrypt hash 返回 false
	_, err := svc.Retrieve(ctx, code, "", "")
	assert.ErrorIs(t, err, ErrPasswordWrong)
}

// TestRetrieve_Unlimited 无限次数可反复取
func TestRetrieve_Unlimited(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "SHARE_INF", FilePath: "uploads/x/SHARE_INF.bin", ExpiredCount: -1}))
	code, _ := svc.GenerateCode(ctx, CodeMeta{ShareCode: "SHARE_INF"}, time.Now().Add(time.Hour))

	for i := 0; i < 5; i++ {
		_, err := svc.Retrieve(ctx, code, "", "")
		require.NoError(t, err, "第 %d 次取件应成功", i+1)
	}
	fc, _ := svc.fileCodeRepo.GetByCode(ctx, "SHARE_INF")
	assert.Equal(t, -1, fc.ExpiredCount)
	assert.Equal(t, 5, fc.UsedCount)
}

// TestCancel 作废取件码
func TestCancel(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "SHARE_CAN", FilePath: "uploads/x/SHARE_CAN.bin", ExpiredCount: -1}))
	code, _ := svc.GenerateCode(ctx, CodeMeta{ShareCode: "SHARE_CAN"}, time.Now().Add(time.Hour))

	require.NoError(t, svc.Cancel(ctx, code))
	_, err := svc.Retrieve(ctx, code, "", "")
	assert.ErrorIs(t, err, ErrCodeNotFound)
}

// TestGenerateCode_ExpiredAtPast expireAt 已过期 → 报错
func TestGenerateCode_ExpiredAtPast(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, err := svc.GenerateCode(context.Background(), CodeMeta{ShareCode: "X"}, time.Now().Add(-time.Hour))
	assert.Error(t, err)
}

// TestRetrieve_ShareCodeFallback 8 位分享码直查兜底：
// 文本分享没有取件码（不写 Redis），用户手里只有分享成功弹窗里的 8 位码，
// 取件页必须能用它取件（回归 2026-10-03 自测：两套码体系在 UI 断链）。
func TestRetrieve_ShareCodeFallback(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "Piqck7ZN", UUIDFileName: "note.txt", Size: 42, FilePath: "uploads/x/note.txt", ExpiredCount: 3,
	}))

	meta, err := svc.Retrieve(ctx, "Piqck7ZN", "", "")
	require.NoError(t, err)
	assert.Equal(t, "Piqck7ZN", meta.ShareCode)
	assert.Equal(t, "note.txt", meta.FileName)
	assert.Equal(t, int64(42), meta.FileSize)

	// 次数照常扣减
	fc, _ := svc.fileCodeRepo.GetByCode(ctx, "Piqck7ZN")
	assert.Equal(t, 2, fc.ExpiredCount)
}

// TestRetrieve_ShareCodeFallback_CaseFold 分享码查询大小写折叠（2026-10-08 起
// 默认开，download.code_case_insensitive 可关）：开启时大小写变体命中；
// 关闭后恢复精确匹配——2026-10-03 的区分大小写语义由该开关承载。
// 两态各用一条码：miss 会写负缓存标记（2min TTL），同码重查会被缓存挡住。
func TestRetrieve_ShareCodeFallback_CaseFold(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "Piqck7ZN", FilePath: "uploads/x/a", ExpiredCount: -1}))
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "Tzqx8WLp", FilePath: "uploads/x/b", ExpiredCount: -1}))

	t.Run("折叠关：大小写必须精确", func(t *testing.T) {
		setCodeFold(t, false)
		_, err := svc.Retrieve(ctx, "PIQCK7ZN", "", "")
		assert.ErrorIs(t, err, ErrCodeNotFound)
	})
	t.Run("折叠开（默认）：变体命中", func(t *testing.T) {
		setCodeFold(t, true)
		meta, err := svc.Retrieve(ctx, "TZQX8WLP", "", "")
		require.NoError(t, err)
		assert.Equal(t, "Tzqx8WLp", meta.ShareCode)
	})
}

// TestRetrieve_ShareCodeFallback_RespectsPassword 兜底路径同样校验密码
func TestRetrieve_ShareCodeFallback_RespectsPassword(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	hash, err := utils.HashPassword("secret1")
	require.NoError(t, err)
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "Passw0rd", PasswordHash: hash, RequireAuth: true, FilePath: "uploads/x/a", ExpiredCount: -1,
	}))

	_, err = svc.Retrieve(ctx, "Passw0rd", "", "")
	assert.ErrorIs(t, err, ErrPasswordWrong)

	meta, err := svc.Retrieve(ctx, "Passw0rd", "secret1", "")
	require.NoError(t, err)
	assert.Equal(t, "Passw0rd", meta.ShareCode)
	assert.True(t, meta.RequireAuth)
}

// TestPeek_ShareCodeFallback Peek（search/下载令牌签发）同样支持分享码兜底
func TestPeek_ShareCodeFallback(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "AbCdEf12", FilePath: "uploads/x/a", ExpiredCount: -1}))

	meta, fc, err := svc.Peek(ctx, "AbCdEf12")
	require.NoError(t, err)
	assert.Equal(t, "AbCdEf12", meta.ShareCode)
	assert.Equal(t, "AbCdEf12", fc.Code)
}

// TestRetrieve_PickupCodeLowercaseInput 6 位取件码容忍小写输入（规范化为大写后查映射）
func TestRetrieve_PickupCodeLowercaseInput(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{Code: "SHARE_LC", FilePath: "uploads/x/a", ExpiredCount: -1}))
	code, err := svc.GenerateCode(ctx, CodeMeta{ShareCode: "SHARE_LC", FileName: "f.bin"}, time.Now().Add(time.Hour))
	require.NoError(t, err)

	meta, err := svc.Retrieve(ctx, strings.ToLower(code), "", "")
	require.NoError(t, err)
	assert.Equal(t, "SHARE_LC", meta.ShareCode)
}
