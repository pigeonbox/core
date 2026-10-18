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

func TestIdentityOf_CacheHitAndInvalidation(t *testing.T) {
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
	SetIdentityLoader(nil)
	t.Cleanup(func() { SetIdentityLoader(nil) })

	rec, err := identityOf(context.Background(), 424244)
	assert.NoError(t, err, "loader 未注入不是错误")
	assert.Nil(t, rec, "loader 未注入返回 (nil,nil)，调用方跳过复核")
}

func TestInvalidateIdentity_LoadingNilSafe(t *testing.T) {
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
	orig := identityCache
	identityCache = newIdentityCache(clock)
	t.Cleanup(func() { identityCache = orig })
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
