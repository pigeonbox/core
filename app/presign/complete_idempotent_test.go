// 上传完成面的**幂等**回归（2026-10-24 审计，core v0.32.6）
//
// 缺陷：chunk/presign 的 Complete **不检查是否已建过分享** ⇒ 同一 upload_id
// 二次 Complete（双击 / 客户端重试 / 网络重试）会**再建一条分享**：
//   - 两个分享各有**独立的取件次数额度**与取件码 ⇒ owner 设的「最多取 N 次」
//     实际可取 **2N** 次（直接击穿取件计次承诺）；
//   - 用户存储统计被双计；
//   - 分片不回收（仅 Cancel 回收）⇒ 第二次必然成功。
//
// 真机实测：13 条带 upload_id 的分享里已有 **1 组重复** ⇒ 已在生产数据发生。
//
// 修法：以 **upload_id 为幂等键** —— 完成面先按 upload_id 回读已建分享，命中即
// 复用（同一个分享码），不新建行、不重复扣次、不重复计配额。
//
// 断言必须能变红（负对照）：
//
//	① 删掉 presign Complete 的幂等回读 ⇒ TestComplete_Idempotent 变红；
//	② 删掉 chunk Complete 的幂等回读 ⇒ TestChunkUploadComplete_Idempotent 变红。
package presign

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockShareService 的幂等语义补强：CreateShare 记下建出的码，
// FindShareByUploadID 回读它（模拟真实 share 域的 upload_id 唯一性）。
// 上面的 mockShareService 已有 existingShareCode 字段，这里直接用。

// TestComplete_Idempotent 同一 upload_id 二次 Complete ⇒ 同一个分享码、
// **只建一次**分享（不新建行 ⇒ 取件额度不翻倍）。
func TestComplete_Idempotent(t *testing.T) {
	svc, _ := newTestService(t)
	mock := &mockShareService{returnCode: "SHARE001", returnURL: "/share/1", returnFull: "http://x/#/share/1"}
	svc.SetShareService(mock)

	ctx := context.Background()
	initRes, err := svc.Init(ctx, InitMeta{FileName: "idem.txt", FileSize: 1024})
	require.NoError(t, err)

	// 第一次 Complete ⇒ 建分享
	first, err := svc.Complete(ctx, initRes.UploadID, initRes.Token, "1.2.3.4")
	require.NoError(t, err)
	require.NotEmpty(t, first.ShareCode)
	assert.Equal(t, 1, mock.called, "首次 Complete 应建一次分享")

	// 模拟「第一次建了分享」：回读命中
	mock.existingShareCode = mock.returnCode

	// 第二次 Complete（同 upload_id）⇒ 幂等：返回**同一个**分享码、不再新建
	second, err := svc.Complete(ctx, initRes.UploadID, initRes.Token, "1.2.3.4")
	require.NoError(t, err, "二次 Complete 应幂等返回，不该报错")
	assert.Equal(t, first.ShareCode, second.ShareCode, "二次 Complete 必须返回同一个分享码")
	assert.Equal(t, 1, mock.called, "二次 Complete 不得再建一条分享（否则取件额度翻倍）")
}

// TestComplete_ShareFailure_NotMarkedComplete 建分享失败 ⇒ **不标记** complete
// ⇒ 客户端重试仍可完成（修复前：先标记 complete 再建分享 ⇒ 失败后重试只得
// ErrAlreadyComplete ⇒ 已上传对象永久成孤儿）。
func TestComplete_ShareFailure_NotMarkedComplete(t *testing.T) {
	svc, _ := newTestService(t)
	mock := &mockShareService{returnErr: errors.New("share db connection lost")}
	svc.SetShareService(mock)

	ctx := context.Background()
	initRes, err := svc.Init(ctx, InitMeta{FileName: "retry.txt", FileSize: 1024})
	require.NoError(t, err)

	_, err = svc.Complete(ctx, initRes.UploadID, initRes.Token, "1.2.3.4")
	require.Error(t, err, "建分享失败应返回错误")

	// 重试：mock 恢复正常 ⇒ 应当能完成（此前因 meta 已标 complete 而失败）
	mock.returnErr = nil
	mock.returnCode = "SHARE002"
	mock.existingShareCode = ""
	retry, err := svc.Complete(ctx, initRes.UploadID, initRes.Token, "1.2.3.4")
	require.NoError(t, err, "建分享失败后重试必须能完成（不得因已标 complete 而永久失败）")
	assert.Equal(t, "SHARE002", retry.ShareCode)
}
