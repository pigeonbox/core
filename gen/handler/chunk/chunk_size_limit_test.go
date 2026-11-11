package chunk

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pigeonbox/contracts/errcode"

	"github.com/pigeonbox/core/storage"
)

// swapStorageChunkLimit 临时改写 storage.MaxChunkBytes（测试换桩；返回恢复函数）。
func swapStorageChunkLimit(t *testing.T, v int64) func() {
	t.Helper()
	old := storage.MaxChunkBytes
	storage.MaxChunkBytes = v
	return func() { storage.MaxChunkBytes = old }
}

// TestChunkUpload_OversizeChunkRejectedBeforeRead（core#140 正判据）：单片超限
// 在读进内存**之前**拒（413 + CodeTooLarge）——否则 make([]byte, file.Size) 的
// 无界整读先发生（core#118 同族 OOM 面）。限内分片照常 200（判据不恒红）。
//
// 判据形状（结构性）：状态码 413 + 业务码 errcode.CodeTooLarge——不是收下后
// 由 storage 入库闸兜底成 500 内部错误。
//
// 负对照（改错转红）：去掉 handler 的 ValidateChunkBytes 前置闸 ⇒ 超限用例
// 转红（落到 storage.SaveChunk 入库闸 ⇒ 500，失败点与状态码都错）。
func TestChunkUpload_OversizeChunkRejectedBeforeRead(t *testing.T) {
	const uploadID = "cafebabe01"
	seedChunkSession(t, uploadID, "big.bin")
	restore := swapStorageChunkLimit(t, 1<<10)
	defer restore()

	code, body := postChunk(t, uploadID, 0, "big.bin", make([]byte, 2<<10))
	assert.Equal(t, http.StatusRequestEntityTooLarge, code,
		"超限分片应在读入内存前拒（413），实际: %v", body)
	assert.Equal(t, float64(errcode.CodeTooLarge), body["code"],
		"业务码应为 CodeTooLarge（不是 10008 内部错误）")

	// 正向对照：限内分片照常收（判据不恒红）
	code2, body2 := postChunk(t, uploadID, 1, "big.bin", []byte("ok"))
	assert.Equal(t, http.StatusOK, code2, "限内分片应正常收: %v", body2)
}
