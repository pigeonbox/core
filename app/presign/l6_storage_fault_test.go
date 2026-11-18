package presign

// l6_storage_fault_test.go —— 直传面遇**存储写失败**的语义守卫（L6 故障注入，
// 矩阵 FI-12）。
//
// 注入什么故障
//   写失败（磁盘满 ENOSPC 等价）——用可控写失败模拟，**不真去填满磁盘**
//   （那会污染整台机器、且不可复现）。
//
// 断言什么（FI-12 的 verdict）
//   写失败后不得出现「账本已提交但对象没落盘」的假成功（blob 账本不得推进到
//   committed）；会话必须保持可重试（meta 不置 Complete ⇒ 客户端可重传同一
//   upload_id）。
//
// 负对照（必须能变红）
//   在写失败分支也推进 committed（或把 committed 提前到写成功之前）
//   ⇒ 本用例转红。这正是本用例要防的缺陷形态（假成功 ⇒ 客户端以为传完了，
//   对象却不存在 ⇒ 数据静默丢失）。
import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// l6FailWriteStorage 写失败存储桩：**嵌入** StorageWriter，只覆写写路径。
//
// 嵌入（而非逐个实现）的好处：接口新增方法时本桩自动跟上。
type l6FailWriteStorage struct {
	StorageWriter
	fail error
}

func (f *l6FailWriteStorage) SaveStream(_ context.Context, _ string, _ io.Reader, _ int64) (int64, error) {
	return 0, f.fail
}

func (f *l6FailWriteStorage) SaveBytes(_ context.Context, _ string, _ []byte) error {
	return f.fail
}

// TestL6_StorageWriteFailure_PresignLedgerNotCommitted 直传写失败时，
// blob 账本**不得**推进到 committed。
//
// 假成功的危害：客户端收到成功 ⇒ 认为对象已落盘 ⇒ 分享建好后下载却是 404
// ⇒ 数据静默丢失且运维无从追查（账本说 committed、对象不存在）。
func TestL6_StorageWriteFailure_PresignLedgerNotCommitted(t *testing.T) {
	svc, g := newUploadStateService(t)
	noSpace := errors.New("注入故障：磁盘满（no space left on device）")
	svc.SetStorage(&l6FailWriteStorage{StorageWriter: newFakeStorage(), fail: noSpace})
	svc.SetShareService(&mockShareService{returnCode: "code"})

	ctx := context.Background()
	body := []byte("l6-write-failure-probe")
	initRes, err := svc.Init(ctx, InitMeta{FileName: "probe.txt", FileSize: int64(len(body))})
	require.NoError(t, err)

	// 注入的写失败必须如实上抛（不得吞成成功）
	err = svc.UploadDirect(ctx, initRes.UploadID, initRes.Token, bytes.NewReader(body), int64(len(body)))
	require.Error(t, err, "写失败必须报错")
	assert.ErrorIs(t, err, noSpace, "底层写失败原因必须保留（不得被吞成笼统错误）")

	// 核心断言：账本行不得处于 committed（假成功）
	row := blobRowOf(t, g, initRes.ObjectKey)
	assert.NotEqual(t, model.BlobCommitted, row.State,
		"写失败后账本不得推进到 committed（否则是「账本已提交但对象没落盘」的假成功）；"+
			"object_key=%s state=%s", row.ObjectKey, row.State)
	// 会话必须保持可重试：meta 未标 complete ⇒ 客户端可重传同一 upload_id
	meta, metaErr := svc.GetMeta(ctx, initRes.UploadID)
	if metaErr == nil && meta != nil {
		assert.False(t, meta.Complete, "写失败后会话不得标记为完成（必须保持可重试）")
	}
}
