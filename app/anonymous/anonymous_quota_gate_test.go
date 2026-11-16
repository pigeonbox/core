package anonymous

// core#169 回归：匿名取件码直传通道的配额闸。
//
// 缺陷：BindFilePath 把占位分享的 size 从 0 抬到实际大小——这是该通道的一次
// 配额扣减，但此前回填完全绕过站点配额闸（checkUploadCaps 只在
// CreateShare/CreateMultiFileShare 入口）⇒ 匿名取件码直传是不经配额的
// 免费通道，且改列与「读合计」之间同样 check-then-act。
//
// 修法：回填走「配额闸」串行段内的单事务（UnderQuotaGateTx：闸内重读合计 +
// 行内旧尺寸 → 判定 → 改列），与文件插入同一把闸。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// setAnonStorageQuota 注入站点配额（0=不限）——经 SetSiteQuotaSource（生产
// 由 bootstrap 注入 conf 读点；service 不直读全局配置，规则 8）。
func setAnonStorageQuota(t *testing.T, svc *Service, q int64) {
	t.Helper()
	svc.SetSiteQuotaSource(func() int64 { return q })
	t.Cleanup(func() { svc.SetSiteQuotaSource(nil) })
}

// seedAnonShare 落一条分享（size 已计入站点用量）。
func seedAnonShare(t *testing.T, svc *Service, code string, size int64) {
	t.Helper()
	require.NoError(t, svc.fileCodeRepo.Create(context.Background(), &model.FileCode{
		Code: code, FilePath: "uploads/seeds/" + code + ".bin", Size: size, Text: code,
		ExpiredAt: ptrTimeA(time.Now().Add(time.Hour)), ExpiredCount: -1,
	}))
}

func ptrTimeA(t time.Time) *time.Time { return &t }

// TestBindFilePath_QuotaGate 超限拒绝 + 未超限放行 + 增量口径（旧尺寸抵扣）。
func TestBindFilePath_QuotaGate(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	seedAnonShare(t, svc, "QG00001", 900)
	setAnonStorageQuota(t, svc, 1000)

	// 占位分享（size=0 待回填）
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "QG00002", UploadType: "anonymous",
		UUIDFileName: "big.bin", ExpiredAt: ptrTimeA(time.Now().Add(time.Hour)), ExpiredCount: -1,
	}))

	// 回填 200 ⇒ 900+200=1100 > 1000，拒绝（修复前该通道无任何配额检查）
	err := svc.BindFilePath(ctx, "QG00002", "uploads/2026/11/16/big.bin", 200, "")
	require.Error(t, err)
	var qe *QuotaExceededError
	require.ErrorAs(t, err, &qe, "应为配额超限 typed error: %v", err)
	require.EqualValues(t, 900, qe.Used)

	// 被拒后不得留下半截状态（file_path 仍空、size 仍 0）
	fc, gerr := svc.fileCodeRepo.GetByCode(ctx, "QG00002")
	require.NoError(t, gerr)
	assert.Empty(t, fc.GetFilePath(), "被拒的回填不得改列")
	assert.EqualValues(t, 0, fc.Size)

	// 回填 100 ⇒ 900+100=1000 ≤ 1000，放行
	require.NoError(t, svc.BindFilePath(ctx, "QG00002", "uploads/2026/11/16/big.bin", 100, ""))
	fc, gerr = svc.fileCodeRepo.GetByCode(ctx, "QG00002")
	require.NoError(t, gerr)
	assert.Equal(t, "uploads/2026/11/16/big.bin", fc.GetFilePath())
	assert.EqualValues(t, 100, fc.Size)

	// quota=0 ⇒ 不限
	setAnonStorageQuota(t, svc, 0)
	require.NoError(t, svc.BindFilePath(ctx, "QG00002", "uploads/2026/11/16/big2.bin", 99999, ""))
}

// TestBindFilePath_IncrementNotAbsolute 回填口径=增量（旧尺寸抵扣）而非绝对值：
// 已回填过的占位被再次回填（异常重放形态）时，判定按 delta 计算，不得把旧
// 尺寸重复计入用量。
func TestBindFilePath_IncrementNotAbsolute(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	seedAnonShare(t, svc, "QI00001", 500)
	setAnonStorageQuota(t, svc, 1000)

	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "QI00002", UploadType: "anonymous",
		UUIDFileName: "a.bin", FilePath: "uploads/2026/11/16/a.bin", Size: 400,
		ExpiredAt: ptrTimeA(time.Now().Add(time.Hour)), ExpiredCount: -1,
	}))

	// 已回填行（size=400）再次回填同尺寸：delta=0 ⇒ 900+0 ≤ 1000 放行。
	// （若误按绝对值判定：900+400=1300 > 1000 会拒绝 ⇒ 本断言即口径判据）
	require.NoError(t, svc.BindFilePath(ctx, "QI00002", "uploads/2026/11/16/a2.bin", 400, ""))

	// 小幅上抬：delta=100 ⇒ 900+100=1000 ≤ 1000 恰好放行
	require.NoError(t, svc.BindFilePath(ctx, "QI00002", "uploads/2026/11/16/a3.bin", 500, ""))

	// 再抬：delta=500 ⇒ 1000+500=1500 > 1000 拒绝
	err := svc.BindFilePath(ctx, "QI00002", "uploads/2026/11/16/a4.bin", 1000, "")
	require.Error(t, err, "增量 500 超过剩余额度必须拒绝")
}
