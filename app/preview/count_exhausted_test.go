// 预览面的「次数耗尽」闸门（2026-10-24）
//
// 预览也是「拿内容」的面（返回文本摘要/图片缩略图元数据）⇒ 与 select/download
// 同口径：额度用完后拒绝，且给**独立错误码**（ErrPickupExhausted ⇒ handler
// 映射 20004「超过取件次数」），不再伪装成 404「分享不存在或已过期」。
//
// 缺口来源同 v0.32.4：`IsExpired()` 拆分前含次数语义 ⇒ 预览面天然也拒「次数
// 耗尽」；拆分后本面**漏了这道闸**（且此前本域**没有任何耗尽态测试** ⇒ 漏了
// 无人发现）。
//
// 断言必须能变红：删掉 app/preview/service.go 里的 IsCountExhausted 闸 ⇒ 本用例红。
package preview

import (
	"context"
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

// TestPreview_CountExhausted_ReturnsReachLimit 额度用完 ⇒ 预览面拒绝。
func TestPreview_CountExhausted_ReturnsReachLimit(t *testing.T) {
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&model.FileCode{}, &model.FilePreview{}))
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	ctx := context.Background()
	// 文件分享、额度已用完（ExpiredCount=0）、时间未过期
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "PREVIEW1", Text: "a.txt", FilePath: "uploads", UUIDFileName: "a.txt",
		Size: 10, ExpiredCount: 0,
	}).Error)

	// storage 传 nil：次数闸在**触碰存储之前**就该拒（否则闸门形同虚设）
	svc := NewService(gormDB, nil)
	_, err = svc.GetOrCreate(ctx, GetReq{Code: "PREVIEW1", ClientIP: "1.2.3.4"})
	require.Error(t, err, "额度用完后预览面必须拒绝（否则仍能看到内容摘要）")
	require.True(t, errors.Is(err, ErrPickupExhausted),
		"应是 ErrPickupExhausted（⇒ handler 映射 20004），实得 %v", err)

	// 额度未用完的同型分享 ⇒ 不被次数闸误伤（负对照：闸门不是「永远拒」）
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "PREVIEW2", Text: "a.txt", FilePath: "uploads", UUIDFileName: "a.txt",
		Size: 10, ExpiredCount: 3,
	}).Error)
	fc, err := svc.fileCodeRepo.GetByCode(ctx, "PREVIEW2")
	require.NoError(t, err)
	require.False(t, fc.IsCountExhausted())
}
