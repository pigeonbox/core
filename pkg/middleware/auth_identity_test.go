package middleware

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 守卫测试：身份回查缓存（2026-10-18 起托管给 oekit/pkg/cache LocalCache）。
// 契约要点：TTL 内命中不回源；InvalidateIdentity 失效后重源；loader 错误
// 不缓存（恒重试）；loader 未注入 → (nil,nil) 纯 JWT 语义。
// TTL 过期行为由 LocalCache 上游契约套件（pkg/cache cachetest）覆盖，此处不睡钟。

// useFreshIdentityCache 给当前用例一份**专属**的身份缓存，退出时还原全局。
//
// ## 为什么必须隔离（2026-10-26 审计）
//
// identityCache 是**包级全局**（生产里就该是全局的），TTL 30s、按真实时钟走
// ⇒ 上一条用例写入的记录会在**下一条用例**里继续命中（同一个 userID 就撞）。
// ⇒ 「TTL 内命中缓存不回源」这类断言只在「进程刚起、缓存是空的」时成立
// ⇒ 依赖运行顺序 ⇒ **`go test -count=2` 必红**（实测：第二轮 calls 计数错位，
// 「TTL 内命中缓存不回源」expected 1 / actual 0、「失效后重新回源」expected 2 /
// actual 1）⇒ CI 偶发红，且红得莫名其妙。
//
// ⇒ 每条触碰身份缓存的用例都先换一份干净缓存（now 非 nil 时注入假时钟）。
func useFreshIdentityCache(t *testing.T, now func() time.Time) {
	t.Helper()
	orig := identityCache
	identityCache = newIdentityCache(now)
	t.Cleanup(func() { identityCache = orig })
}

func TestIdentityOf_CacheHitAndInvalidation(t *testing.T) {
	useFreshIdentityCache(t, nil)
	calls := 0
	SetIdentityLoader(func(ctx context.Context, userID uint) (*IdentityRecord, error) {
		calls++
		return &IdentityRecord{Status: "active", Role: "user", Epoch: 7}, nil
	})
	t.Cleanup(func() { SetIdentityLoader(nil) })
	ctx := context.Background()

	rec1, err := identityOf(ctx, 424242)
	require.NoError(t, err)
	require.NotNil(t, rec1)
	assert.Equal(t, "active", rec1.Status)

	_, err = identityOf(ctx, 424242)
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "TTL 内命中缓存不回源")

	InvalidateIdentity(424242)
	_, err = identityOf(ctx, 424242)
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "失效后重新回源")

	assert.NotSame(t, rec1, mustIdentity(t, ctx, 424242, &calls), "重源后拿到新快照")
}

// mustIdentity 便捷取数（断言无错后返回记录）
func mustIdentity(t *testing.T, ctx context.Context, uid uint, calls *int) *IdentityRecord {
	t.Helper()
	rec, err := identityOf(ctx, uid)
	require.NoError(t, err)
	require.NotNil(t, rec)
	return rec
}

func TestIdentityOf_LoaderErrorNotCached(t *testing.T) {
	useFreshIdentityCache(t, nil)
	calls := 0
	SetIdentityLoader(func(ctx context.Context, userID uint) (*IdentityRecord, error) {
		calls++
		return nil, errors.New("db down")
	})
	t.Cleanup(func() { SetIdentityLoader(nil) })
	ctx := context.Background()

	_, err1 := identityOf(ctx, 424243)
	_, err2 := identityOf(ctx, 424243)
	assert.Error(t, err1)
	assert.Error(t, err2)
	assert.Equal(t, 2, calls, "loader 错误不得缓存，每次重试")
}

func TestIdentityOf_LoaderNil_PureJWTSemantics(t *testing.T) {
	useFreshIdentityCache(t, nil)
	SetIdentityLoader(nil)
	t.Cleanup(func() { SetIdentityLoader(nil) })

	rec, err := identityOf(context.Background(), 424244)
	assert.NoError(t, err, "loader 未注入不是错误")
	assert.Nil(t, rec, "loader 未注入返回 (nil,nil)，调用方跳过复核")
}

func TestInvalidateIdentity_LoadingNilSafe(t *testing.T) {
	useFreshIdentityCache(t, nil)
	// loader 未注入时失效调用也不得 panic（改密/封禁路径在轻量环境同样会走）
	assert.NotPanics(t, func() {
		SetIdentityLoader(nil)
		InvalidateIdentity(424245)
	})
}

// 守卫测试：TTL 过期边界（假时钟确定性，pkg v0.13.0 #9 LocalConfig.Now 落地后
// 收回此前「押注上游契约套件」的断言）。契约语义：恰到期仍命中（闭区间），
// 越过 1ns 即 miss 重源。
func TestIdentityOf_TTLExpiry_FakeClock(t *testing.T) {
	calls := 0
	SetIdentityLoader(func(ctx context.Context, userID uint) (*IdentityRecord, error) {
		calls++
		return &IdentityRecord{Status: "active", Role: "user", Epoch: 1}, nil
	})
	t.Cleanup(func() { SetIdentityLoader(nil) })

	cur := time.Now()
	clock := func() time.Time { return cur }
	useFreshIdentityCache(t, clock)
	ctx := context.Background()

	_, _ = identityOf(ctx, 424246)
	_, _ = identityOf(ctx, 424246)
	assert.Equal(t, 1, calls, "TTL 内命中不回源")

	cur = cur.Add(identityCacheTTL)
	_, _ = identityOf(ctx, 424246)
	assert.Equal(t, 1, calls, "恰到期边界仍命中（闭区间契约）")

	cur = cur.Add(time.Nanosecond)
	_, _ = identityOf(ctx, 424246)
	assert.Equal(t, 2, calls, "越过过期点即重源")
}
