package chunk

// l6_storage_fault_test.go —— 分片通道遇**存储失联**（读路径）的语义守卫
// （L6 故障注入，矩阵 FI-18）。
//
// 注入什么故障
//   合并产物的读取路径失联（GetFileReader 报后端不可达）——用可控故障桩注入，
//   **不真去停远端进程**。
//
// 为什么这条值得单列（与 FI-07/FI-08 的分工）
//   FI-07 锁的是**存储层**契约（FileExists/ObjectExists 不得吞错成「不存在」），
//   FI-08 锁的是**取件面**不得把存储失联伪装成 404。
//   本条锁**分片通道的读路径**：合并后的对象在「哈希计算/魔数复检」阶段要
//   被重新打开读流；此时后端失联必须**如实上抛**，不得：
//     · 吞成「文件为空/校验通过」⇒ 魔数复检放行任意内容（安全洞）；
//     · 吞成「文件不存在」⇒ 运维按「文件丢了」排查。
//
// 负对照（必须能变红）
//   把 OpenMerged 的存储错误吞掉（返回空流、nil error）
//   ⇒ 本用例转红。这正是本用例要防的缺陷形态。
import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/storage"
)

// l6UnreachableStorage 读路径失联的存储桩：**嵌入**真实接口，只覆写读路径。
type l6UnreachableStorage struct {
	storage.StorageInterface
	failRead error
}

func (f *l6UnreachableStorage) GetFileReader(_ context.Context, _ string) (io.ReadCloser, int64, error) {
	return nil, 0, f.failRead
}

func (f *l6UnreachableStorage) GetFile(_ context.Context, _ string) ([]byte, error) {
	return nil, f.failRead
}

// TestL6_StorageUnreachable_ChunkMergedReadDoesNotDisguiseAsEmpty 合并产物读取
// 时遇存储失联，必须**如实上抛**：不得伪装成「文件为空/校验通过」，
// 也不得伪装成「文件不存在」。
func TestL6_StorageUnreachable_ChunkMergedReadDoesNotDisguiseAsEmpty(t *testing.T) {
	svc, _, _ := newFaultChunkService(t)
	backendDown := errors.New("注入故障：存储后端不可达（connection refused）")
	svc.SetStorage(&l6UnreachableStorage{
		StorageInterface: svc.storageClient(),
		failRead:         backendDown,
	})

	rc, size, err := svc.OpenMerged(context.Background(), "uploads/2026/11/18/merged.bin")

	require.Error(t, err, "存储失联必须报错，不能返回空流")
	assert.Nil(t, rc, "存储失联时不得返回可用读取器（否则魔数复检会放行空内容）")
	assert.Zero(t, size, "存储失联时不得报告出可读大小")
	// 不得伪装成「资源不存在」
	assert.False(t, errors.Is(err, os.ErrNotExist),
		"存储失联不得伪装成「文件不存在」（404 语义）；实际错误: %v", err)
	// 底层故障信息必须保留
	assert.ErrorIs(t, err, backendDown,
		"底层存储错误必须原样上抛（不得被吞成笼统错误）")
}
