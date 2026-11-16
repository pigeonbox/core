// core#219：删除用户的审计覆盖（不可逆数据销毁必须可追溯）。
package admin

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/pkg/audit"
	"github.com/pigeonbox/core/repo/db/model"
)

type userAuditSink struct{ entries []audit.Entry }

func (s *userAuditSink) Create(_ context.Context, e audit.Entry) error {
	s.entries = append(s.entries, e)
	return nil
}

func TestDeleteUserWritesAudit(t *testing.T) {
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.User{}, &model.FileCode{}, &model.FileCodeFile{}))

	sink := &userAuditSink{}
	audit.SetSink(sink)
	t.Cleanup(func() { audit.SetSink(nil) })

	svc := NewService(g)
	ctx := context.Background()
	u := &model.User{Username: "victim", Email: "victim@example.com", Role: "user", Status: "active"}
	require.NoError(t, svc.userRepo.Create(ctx, u))
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "AUDIT001", UserID: &u.ID, ExpiredCount: -1,
	}))

	require.NoError(t, svc.DeleteUser(ctx, u.ID))

	require.NotEmpty(t, sink.entries)
	var found bool
	for _, e := range sink.entries {
		if e.Action != "user.delete" {
			continue
		}
		found = true
		assert.True(t, e.Success)
		assert.Contains(t, e.Target, "user")
		assert.Contains(t, e.Target, "victim", "审计必须能还原「删的是谁」")
	}
	assert.True(t, found, "删除用户必须写 user.delete 审计：%+v", sink.entries)
}
