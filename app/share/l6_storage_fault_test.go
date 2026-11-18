package share

// l6_storage_fault_test.go —— 取件/下载面遇**存储失联**的语义守卫（L6 故障注入，
// 矩阵 FI-08）。
//
// 注入什么故障
//   存储后端不可达（GetFileReader 报连接失败）——用可控故障存储桩注入，
//   **不真去拔网线/停进程**（那会污染环境且不可复现）。
//
// 断言什么（FI-08 的 verdict）
//   「基建故障」与「资源不存在」必须可区分：存储失联时不得回 404 语义
//   （否则运维按「文件丢了」排查，且存在性探测面失真）。
//   本组用例锁住「错误原样上抛」这一层事实（HTTP 层的错误码映射见 KG-01）。
//
// 负对照（必须能变红）
//   把 OpenShareDownload 里的存储错误改写成 os.ErrNotExist（⇒ 伪装成 404）
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

// l6FaultStorage 故障存储桩：**嵌入**真实接口，只覆写读路径。
//
// 嵌入（而非逐个实现）的好处：接口新增方法时本桩自动跟上，不会因方法集
// 缺项而编译失败——故障桩只关心「读路径注入故障」这一件事。
type l6FaultStorage struct {
	storage.StorageInterface
	failRead error
}

func (f *l6FaultStorage) GetFileReader(_ context.Context, _ string) (io.ReadCloser, int64, error) {
	return nil, 0, f.failRead
}

func (f *l6FaultStorage) GetFile(_ context.Context, _ string) ([]byte, error) {
	return nil, f.failRead
}

// TestL6_StorageUnreachable_ShareDomainDoesNotDisguiseAsNotFound 存储失联时，
// 取件面必须把错误**如实上抛**，不得伪装成「文件不存在」（404 语义）。
//
// 这是运维可观测性的地基：存储故障被伪装成 404，排查会被引向「文件丢了」
// 而非「后端挂了」，且存在性探测面被失真。
func TestL6_StorageUnreachable_ShareDomainDoesNotDisguiseAsNotFound(t *testing.T) {
	// 内层用真实存储实现（不注入的其余操作走它）；本用例只碰读路径。
	inner := &storage.StorageService{}
	svc := NewService(nil, "http://test.local", inner)

	// 注入「存储后端不可达」：连接失败（等价：远端对象存储宕机 / 网络不通）
	backendDown := errors.New("注入故障：存储后端不可达（connection refused）")
	svc.storage = &l6FaultStorage{StorageInterface: inner, failRead: backendDown}

	_, err := svc.OpenShareDownload(context.Background(), "2026/11/18/does-not-matter.bin")

	require.Error(t, err, "存储失联必须报错，不能返回空 payload")
	// 核心断言：不得被伪装成「资源不存在」
	assert.False(t, errors.Is(err, os.ErrNotExist),
		"存储失联不得伪装成「文件不存在」（404 语义）；实际错误: %v", err)
	// 底层故障信息必须保留（运维要能看到真因）
	assert.ErrorIs(t, err, backendDown,
		"底层存储错误必须原样上抛（不得被吞成笼统错误）")
}
