package share

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 唯一冲突判据的 sqlite 路径回归（2026-10-27 收编到 dao.IsUniqueViolation 时补）。
//
// 背景：core 默认库是 glebarez/sqlite（纯 Go 驱动）⇒ 撞唯一索引时错误**既不映射**
// gorm.ErrDuplicatedKey（core 未开 TranslateError）、文案也是
// "UNIQUE constraint failed: …" ⇒ 此前 app/share 的两处判定只认 gorm 映射
// ⇒ sqlite 下**判不出来**（裸 SQL 错误漏到用户面前；换码重试形同虚设）。
//
// 本用例用**真 sqlite**（内存库）造一次真实唯一冲突 ⇒ 锁住收编后的行为：
// 业务错误文案，不是裸 SQL 报错。
func TestCustomCodeDuplicateOnSQLiteMapsToBusinessError(t *testing.T) {
	svc, _, _, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()

	const custom = "DUPCODE123"
	// 第一次：正常创建
	_, err := svc.CreateShare(ctx, &ShareFileReq{
		FilePath:   "/tmp/a.txt",
		Size:       3,
		Text:       "a",
		CustomCode: custom,
	})
	require.NoError(t, err)

	// 第二次：同一自定义码 ⇒ 撞 file_codes 唯一索引（sqlite 路径）
	_, err = svc.CreateShare(ctx, &ShareFileReq{
		FilePath:   "/tmp/b.txt",
		Size:       3,
		Text:       "b",
		CustomCode: custom,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "自定义取件码已被占用",
		"sqlite 唯一冲突必须映射成业务错误，不能漏出裸 SQL 报错（实际: %v)", err)
	assert.NotContains(t, err.Error(), "UNIQUE constraint failed",
		"裸 SQL 报错不应出现在用户可见文案里")
	assert.False(t, strings.Contains(err.Error(), "constraint failed"),
		"判据必须窄：只认唯一冲突文案，不把普通约束错误当唯一冲突")
}
