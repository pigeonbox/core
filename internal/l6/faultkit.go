// Package l6 是 L6 层（故障注入）的**统一注入面**（注入工具箱）。
//
// # 为什么 L6 需要一个「统一注入面」
//
// L6（故障注入整层）此前只有**零散**用例：presign 的 Redis 降级、备份导出吞后端
// 故障、chunk 写失败……每个域各自造桩（miniredis Close、假存储、假 operator），
// 代价是：
//
//	· 同一种故障（Redis 失联 / 磁盘满 / 存储失联）在不同域**注入方式不一致**，
//	  新域要重新发明一遍；
//	· 「注入 ⇒ 期望的降级/错误语义」只存在于用例注释里，**不可机器核对**；
//	· 没有覆盖真相源 ⇒ 看不出哪类故障有覆盖、哪类没有。
//
// 本包把三件事收成一处：
//
//	① 故障分类（Kind）与期望语义（Mode）两套**受控词表**——清单 pkg/l6/matrix.json
//	  只能引用这些词 ⇒ 「期望语义」变成可机械核对的字段，而不是散文；
//	② 可复用的注入原语（FaultStorage：逐操作注入写失败/失联；RedisFixture：
//	  Down/Up 制造 Redis 宕机与恢复）；
//	③ 断言词汇（TypedCodeOf / RequireTypedCode）：core 的 typed error 约定
//	  （实现 ErrCode() int ⇒ handler 透传业务码）由工具箱统一封装，
//	  用例不必各写一遍 errors.As 断言。
//
// # 为什么落在 internal 而不是 pkg
//
// core 的架构守卫（internal/arch 规则 1）禁止 **pkg/** import storage —— 而故障
// 注入的核心原语必须实现 storage.StorageInterface ⇒ 只能落在 internal/（本仓内
// 可见、外部不可 import）。清单与判定层在 pkg/l6/（与 L8 同构），工具箱在本包。
//
// # 词表与清单的关系
//
// Kind/Mode 是**唯一真相源**（本包），pkg/l6/matrix_guard_test.go 拿它们核对
// pkg/l6/matrix.json：清单里出现词表外的值 ⇒ 红（写错字、写了自造词都跑不掉）。
package l6

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/pigeonbox/core/storage"
)

// ==================== 故障类型（受控词表） ====================
//
// Kind 是「注入了哪一种故障」的分类键。清单 pkg/l6/matrix.json 的每条用例必须
// 引用其中之一 ⇒ 「L6 覆盖了哪几类故障」是机器可读的，不是靠人回忆。

// Kind 故障类型。
type Kind string

const (
	// KindStorageUnreachable 存储后端失联（连接失败/不可达/进程被杀）。
	KindStorageUnreachable Kind = "storage_unreachable"
	// KindStorageWriteFail 写失败（磁盘满 ENOSPC 等价；**用可控写失败模拟，不真填满磁盘**）。
	KindStorageWriteFail Kind = "storage_write_fail"
	// KindRemoteUnavailable 远端后端 5xx / 远端服务不可用。
	KindRemoteUnavailable Kind = "remote_unavailable"
	// KindRedisDown Redis 失联（连接失败/宕机）。
	KindRedisDown Kind = "redis_down"
	// KindDBUnavailable 数据库不可用（连接断/写入报错）。
	KindDBUnavailable Kind = "db_unavailable"
)

// AllKinds 全量故障类型（受控词表的唯一真相源）。
var AllKinds = []Kind{
	KindStorageUnreachable,
	KindStorageWriteFail,
	KindRemoteUnavailable,
	KindRedisDown,
	KindDBUnavailable,
}

// ==================== 期望语义（受控词表） ====================

// Mode 是「注入之后必须成立的语义」——L6 的断言词汇。
//
// 每个 Mode 都对应一句可判定的断言：
//
//	ModeDegrade     必须走**降级路径**（不得让整面不可用）
//	ModeTypedError  必须返回**带业务码的类型化错误**（不是笼统 500）
//	ModeNoHalfState 不得留**半成品/半状态**（对象、分享行、账本、预留）
//	ModeRecover     故障恢复后必须**自动切回**正常路径
//	ModeRejectClean 必须**明确拒绝**，不得伪装成别的语义（如「不存在」）
type Mode string

const (
	// ModeDegrade 必须走降级路径。
	ModeDegrade Mode = "degrade"
	// ModeTypedError 必须返回带业务码的类型化错误。
	ModeTypedError Mode = "typed_error"
	// ModeNoHalfState 不得留半成品/半状态。
	ModeNoHalfState Mode = "no_half_state"
	// ModeRecover 故障恢复后必须自动切回正常路径。
	ModeRecover Mode = "recover"
	// ModeRejectClean 必须明确拒绝，不得伪装成别的语义。
	ModeRejectClean Mode = "reject_clean"
)

// AllModes 全量期望语义（受控词表的唯一真相源）。
var AllModes = []Mode{
	ModeDegrade,
	ModeTypedError,
	ModeNoHalfState,
	ModeRecover,
	ModeRejectClean,
}

// ==================== 注入用哨兵错误 ====================

var (
	// ErrNoSpace 磁盘满（ENOSPC 等价）。
	//
	// 「磁盘满」在 L6 里**用可控的写失败模拟**，绝不真去填满磁盘（那会污染
	// 整台机器、且不可复现）。凡是需要「写失败」的注入点一律用它。
	ErrNoSpace = errors.New("注入故障：磁盘满（no space left on device）")

	// ErrBackendDown 存储/远端后端失联。
	ErrBackendDown = errors.New("注入故障：存储后端不可达")

	// ErrDBDown 数据库不可用。
	ErrDBDown = errors.New("注入故障：数据库不可达")
)

// ==================== 操作名（注入点词表） ====================

// 可注入故障的存储操作名（FaultStorage.FailOp 的参数）。
const (
	OpSaveFile      = "SaveFile"
	OpSaveStream    = "SaveStream"
	OpSaveBytes     = "SaveBytes"
	OpSaveChunk     = "SaveChunk"
	OpMergeChunks   = "MergeChunks"
	OpGetFile       = "GetFile"
	OpGetFileReader = "GetFileReader"
	OpGetFileSize   = "GetFileSize"
	OpGetFileURL    = "GetFileURL"
	OpDeleteFile    = "DeleteFile"
	OpCleanChunks   = "CleanChunks"
	OpFileExists    = "FileExists"
)

// ==================== FaultStorage ====================

// FaultStorage 可注入故障的存储实现。
//
// 包住任意 storage.StorageInterface，按**操作**逐个注入失败；未注入的操作
// 照常委派给底层实现（base 为 nil 时返回零值——便于只关心故障路径的用例）。
//
// 额外提供 SaveBytes：app/presign 的 StorageWriter 是 SaveBytes+DeleteFile 的
// 两方法窄接口（刻意不反向依赖存储包），本包装器同时满足它 ⇒ 直传面的写失败
// 注入也能用同一个夹具。
type FaultStorage struct {
	storage.StorageInterface

	mu    sync.Mutex
	fails map[string]error
	calls map[string]int
}

// NewFaultStorage 构造可注入故障的存储包装器。
//
// base 为 nil ⇒ 未注入的操作返回零值（不 panic），便于「只测故障路径」的用例。
func NewFaultStorage(base storage.StorageInterface) *FaultStorage {
	return &FaultStorage{
		StorageInterface: base,
		fails:            make(map[string]error),
		calls:            make(map[string]int),
	}
}

// FailOp 注入故障：op 之后一律返回 err。
func (f *FaultStorage) FailOp(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[op] = err
}

// ClearOp 撤销注入。
func (f *FaultStorage) ClearOp(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fails, op)
}

// Calls 返回该操作被调用的次数（并发安全）。
func (f *FaultStorage) Calls(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

// err 查注入表并记账（返回注入的错误；无注入则 nil）。
func (f *FaultStorage) err(op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[op]++
	return f.fails[op]
}

// SaveFile 存储：注入点 SaveFile。
func (f *FaultStorage) SaveFile(ctx context.Context, fh *multipart.FileHeader, savePath string) (*storage.FileOperationResult, error) {
	if err := f.err(OpSaveFile); err != nil {
		return nil, err
	}
	if f.StorageInterface == nil {
		return &storage.FileOperationResult{}, nil
	}
	return f.StorageInterface.SaveFile(ctx, fh, savePath)
}

// SaveStream 存储：注入点 SaveStream（磁盘满走这里）。
func (f *FaultStorage) SaveStream(ctx context.Context, savePath string, r io.Reader, expectedSize int64) (int64, error) {
	if err := f.err(OpSaveStream); err != nil {
		return 0, err
	}
	if f.StorageInterface == nil {
		return 0, nil
	}
	return f.StorageInterface.SaveStream(ctx, savePath, r, expectedSize)
}

// SaveBytes 存储：注入点 SaveBytes（presign 的直传写入走这条）。
func (f *FaultStorage) SaveBytes(ctx context.Context, savePath string, data []byte) error {
	if err := f.err(OpSaveBytes); err != nil {
		return err
	}
	if f.StorageInterface == nil {
		return nil
	}
	type byteSaver interface {
		SaveBytes(ctx context.Context, savePath string, data []byte) error
	}
	if bs, ok := f.StorageInterface.(byteSaver); ok {
		return bs.SaveBytes(ctx, savePath, data)
	}
	return nil
}

// SaveChunk 分片写入：注入点 SaveChunk。
func (f *FaultStorage) SaveChunk(ctx context.Context, uploadID string, chunkIndex int, data []byte) error {
	if err := f.err(OpSaveChunk); err != nil {
		return err
	}
	if f.StorageInterface == nil {
		return nil
	}
	return f.StorageInterface.SaveChunk(ctx, uploadID, chunkIndex, data)
}

// MergeChunks 分片合并：注入点 MergeChunks。
func (f *FaultStorage) MergeChunks(ctx context.Context, uploadID string, totalChunks int, savePath string) error {
	if err := f.err(OpMergeChunks); err != nil {
		return err
	}
	if f.StorageInterface == nil {
		return nil
	}
	return f.StorageInterface.MergeChunks(ctx, uploadID, totalChunks, savePath)
}

// GetFile 读取：注入点 GetFile。
func (f *FaultStorage) GetFile(ctx context.Context, filePath string) ([]byte, error) {
	if err := f.err(OpGetFile); err != nil {
		return nil, err
	}
	if f.StorageInterface == nil {
		return nil, nil
	}
	return f.StorageInterface.GetFile(ctx, filePath)
}

// GetFileReader 流式读取：注入点 GetFileReader（存储失联 ⇒ 取件/下载面）。
func (f *FaultStorage) GetFileReader(ctx context.Context, filePath string) (io.ReadCloser, int64, error) {
	if err := f.err(OpGetFileReader); err != nil {
		return nil, 0, err
	}
	if f.StorageInterface == nil {
		return nil, 0, nil
	}
	return f.StorageInterface.GetFileReader(ctx, filePath)
}

// GetFileSize 取大小：注入点 GetFileSize。
func (f *FaultStorage) GetFileSize(ctx context.Context, filePath string) (int64, error) {
	if err := f.err(OpGetFileSize); err != nil {
		return 0, err
	}
	if f.StorageInterface == nil {
		return 0, nil
	}
	return f.StorageInterface.GetFileSize(ctx, filePath)
}

// GetFileURL 取直链：注入点 GetFileURL。
func (f *FaultStorage) GetFileURL(ctx context.Context, filePath string) (string, error) {
	if err := f.err(OpGetFileURL); err != nil {
		return "", err
	}
	if f.StorageInterface == nil {
		return "", nil
	}
	return f.StorageInterface.GetFileURL(ctx, filePath)
}

// DeleteFile 删除：注入点 DeleteFile。
func (f *FaultStorage) DeleteFile(ctx context.Context, filePath string) error {
	if err := f.err(OpDeleteFile); err != nil {
		return err
	}
	if f.StorageInterface == nil {
		return nil
	}
	return f.StorageInterface.DeleteFile(ctx, filePath)
}

// CleanChunks 清理分片：注入点 CleanChunks。
func (f *FaultStorage) CleanChunks(ctx context.Context, uploadID string) error {
	if err := f.err(OpCleanChunks); err != nil {
		return err
	}
	if f.StorageInterface == nil {
		return nil
	}
	return f.StorageInterface.CleanChunks(ctx, uploadID)
}

// FileExists 存在性探测：注入点 FileExists。
//
// ⚠️ StorageInterface.FileExists 的签名**没有 error** ⇒ 后端失联时无法如实
// 上抛，只能返回 false（「看起来不存在」）——这正是 L6 要盯的「失联伪装成不存在」
// 陷阱。故本包另提供 ObjectExists（错误可见的存在性探测），用例应优先用它。
func (f *FaultStorage) FileExists(ctx context.Context, filePath string) bool {
	if err := f.err(OpFileExists); err != nil {
		return false
	}
	if f.StorageInterface == nil {
		return false
	}
	return f.StorageInterface.FileExists(ctx, filePath)
}

// ObjectExists 错误可见的存在性探测（storage.StorageService 的同名方法）。
//
// 真不存在 ⇒ (false, nil)；后端失联 ⇒ (false, err)——**不得**把失联吞成
// 「不存在」（那是枚举面与排障面都会失真的错误语义）。
func (f *FaultStorage) ObjectExists(ctx context.Context, filePath string) (bool, error) {
	if err := f.err(OpFileExists); err != nil {
		return false, err
	}
	if f.StorageInterface == nil {
		return false, nil
	}
	type objectExistser interface {
		ObjectExists(ctx context.Context, filePath string) (bool, error)
	}
	if oe, ok := f.StorageInterface.(objectExistser); ok {
		return oe.ObjectExists(ctx, filePath)
	}
	return f.StorageInterface.FileExists(ctx, filePath), nil
}

// ==================== RedisFixture ====================

// RedisFixture Redis 故障注入夹具：Down 制造失联、Up 制造恢复。
//
// 用 miniredis（进程内真 Redis 协议实现）而不是 mock client ⇒ 注入的是
// **真实的连接层故障**（拨号失败/超时），不是「假对象返回假错误」。
type RedisFixture struct {
	mr     *miniredis.Miniredis
	client *redis.Client
}

// NewRedisFixture 起一个 miniredis 并返回快速失败的 go-redis 客户端。
//
// 参数刻意收紧（MaxRetries=-1 / DialerRetries=1 / 短超时）：故障注入必须
// **快速失败**，否则单条命令的耗时会把断言变成时序竞态（照抄
// pkg/middleware/ratelimit_periodlimit_test.go 的同款结论）。
func NewRedisFixture(t testing.TB) *RedisFixture {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{
		Addr:          mr.Addr(),
		MaxRetries:    -1,
		DialerRetries: 1,
		DialTimeout:   10 * time.Millisecond,
		ReadTimeout:   2 * time.Second,
		WriteTimeout:  2 * time.Second,
	})
	t.Cleanup(func() { _ = c.Close() })
	return &RedisFixture{mr: mr, client: c}
}

// Client 返回夹具持有的 go-redis 客户端。
func (f *RedisFixture) Client() *redis.Client { return f.client }

// Miniredis 返回底层 miniredis 实例（需要直接操作键时用）。
func (f *RedisFixture) Miniredis() *miniredis.Miniredis { return f.mr }

// Down 制造 Redis 失联（停服）。
func (f *RedisFixture) Down() { f.mr.Close() }

// Up 制造 Redis 恢复（重启同一实例，数据保留）。
func (f *RedisFixture) Up() { f.mr.Restart() }

// ==================== 断言词汇 ====================

// TypedCodeOf 取错误携带的业务码。
//
// core 的 typed error 约定：实现 ErrCode() int 的错误 ⇒ handler 侧透传业务码
// （pkg/resp.NewTypedError）。未实现该接口的错误一律**不**带业务码
// （会被兜底成 10008 内部错误 ⇒ 前端看到「服务器内部错误」掩盖真相）。
func TypedCodeOf(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var coder interface{ ErrCode() int }
	if errors.As(err, &coder) {
		return coder.ErrCode(), true
	}
	return 0, false
}

// RequireTypedCode 断言 err 携带期望业务码（typed error 约定）。
//
// 这是 L6「存储失联 ⇒ 明确错误码而非 500 空错」这条期望的标准断言方式。
func RequireTypedCode(t testing.TB, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误携带业务码 %d，实际 err 为 nil", want)
	}
	got, ok := TypedCodeOf(err)
	if !ok {
		t.Fatalf("错误未携带业务码（未实现 ErrCode() int，handler 会兜底成内部错误）：%v", err)
	}
	if got != want {
		t.Fatalf("错误业务码不符：期望 %d，实际 %d（%v）", want, got, err)
	}
}
