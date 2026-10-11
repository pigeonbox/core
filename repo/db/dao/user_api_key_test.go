package dao

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// UserAPIKey DAO 临期扫描测试（sqlite :memory: 钉单连接）。
func newAPIKeyDAOTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.User{}, &model.UserAPIKey{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

func insertKey(t *testing.T, g *gorm.DB, mutate func(*model.UserAPIKey)) *model.UserAPIKey {
	t.Helper()
	k := &model.UserAPIKey{UserID: 1, Name: "t", Prefix: "pb_sk_te", KeyHash: "h" + time.Now().Format("150405.000000000")}
	if mutate != nil {
		mutate(k)
	}
	require.NoError(t, NewUserAPIKeyRepository(g).Create(context.Background(), k))
	return k
}

func TestListExpiringWithin_Filters(t *testing.T) {
	g := newAPIKeyDAOTestDB(t)
	ctx := context.Background()
	in3d := time.Now().Add(72 * time.Hour)
	notified := time.Now()

	insertKey(t, g, func(k *model.UserAPIKey) { k.ExpiresAt = &in3d })                                 // 命中
	insertKey(t, g, func(k *model.UserAPIKey) { k.ExpiresAt = &in3d; k.ExpiryNotifiedAt = &notified }) // 已通知 → 排除
	insertKey(t, g, func(k *model.UserAPIKey) { k.ExpiresAt = &in3d; k.Revoked = true })               // 已吊销 → 排除
	insertKey(t, g, nil)                                                                               // 永久 → 排除
	far := time.Now().Add(30 * 24 * time.Hour)
	insertKey(t, g, func(k *model.UserAPIKey) { k.ExpiresAt = &far }) // 30 天后 → 排除

	keys, err := NewUserAPIKeyRepository(g).ListExpiringWithin(ctx, time.Now().Add(7*24*time.Hour))
	require.NoError(t, err)
	assert.Len(t, keys, 1, "只应命中未通知、未吊销、7 天内到期的 Key")

	// 标记后不再命中
	require.NoError(t, NewUserAPIKeyRepository(g).MarkExpiryNotified(ctx, keys[0].ID))
	keys2, err := NewUserAPIKeyRepository(g).ListExpiringWithin(ctx, time.Now().Add(7*24*time.Hour))
	require.NoError(t, err)
	assert.Empty(t, keys2)
}

func TestTouchLastUsed_RecordsIP(t *testing.T) {
	g := newAPIKeyDAOTestDB(t)
	ctx := context.Background()
	k := insertKey(t, g, nil)

	require.NoError(t, NewUserAPIKeyRepository(g).TouchLastUsed(ctx, k.ID, "203.0.113.9"))
	got, err := NewUserAPIKeyRepository(g).GetByID(ctx, k.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastUsedAt)
	assert.Equal(t, "203.0.113.9", got.LastUsedIP)
}
