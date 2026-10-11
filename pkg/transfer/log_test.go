package transfer

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// daoSink 测试桥：落盘能力接 dao（生产由 bootstrap 注入同形实现）。
type daoSink struct{ gormDB *gorm.DB }

func (s daoSink) Create(ctx context.Context, e Entry) error {
	return dao.NewTransferLogRepository(s.gormDB).Create(ctx, &model.TransferLog{
		Operation: e.Operation, FileCodeID: e.FileCodeID, FileCode: e.Code,
		FileName: e.FileName, FileSize: e.FileSize, UserID: e.UserID,
		APIKeyID: e.APIKeyID, Username: e.Username, IP: e.IP, DurationMs: e.DurationMs,
	})
}

// TestRecord_PersistsAttribution Record 落库（含 API Key 归因列）。
// sqlite :memory: 多连接各见独立库，钉死单连接；Record 为异步，轮询等待。
func TestRecord_PersistsAttribution(t *testing.T) {
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.TransferLog{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	SetSink(daoSink{gormDB: g})
	t.Cleanup(func() { Flush(3 * time.Second); SetSink(nil) })

	uid, keyID := uint(9), uint(4)
	Record(Entry{
		Operation: OpUpload, FileCodeID: 1, Code: "AbCd1234", FileName: "a.txt",
		FileSize: 42, UserID: &uid, APIKeyID: &keyID,
		Username: "alice", IP: "10.0.0.1",
	})

	var got model.TransferLog
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		items, _, err := dao.NewTransferLogRepository(g).List(
			context.Background(), model.TransferLogQuery{Operation: OpUpload})
		if err == nil && len(items) > 0 {
			got = *items[0]
			assert.Equal(t, "AbCd1234", got.FileCode)
			require.NotNil(t, got.UserID)
			assert.Equal(t, uid, *got.UserID)
			require.NotNil(t, got.APIKeyID, "Key 认证的操作必须落 api_key_id 归因")
			assert.Equal(t, keyID, *got.APIKeyID)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("3s 内未落库")
}
