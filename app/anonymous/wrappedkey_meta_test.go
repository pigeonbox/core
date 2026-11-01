package anonymous

import (
	"testing"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
)

// TestEnrichMetaFromDB_WrappedKey 口令型 E2E（core#30）：取件展示信息回填
// wrapped_key（不进 KV meta 串，真相源=DB）。
// handler 据此在 RetrieveData 回传信封密钥，取件端走口令派生 KEK 解封。
func TestEnrichMetaFromDB_WrappedKey(t *testing.T) {
	// 有 wrapped_key ⇒ 回填
	meta := &CodeMeta{ShareCode: "WKEY01"}
	fc := &model.FileCode{Code: "WKEY01", Encrypted: true, WrappedKey: "AQ==envelope"}
	enrichMetaFromDB(meta, fc)
	assert.Equal(t, "AQ==envelope", meta.WrappedKey)

	// 无 wrapped_key（旧 URL-hash-key 形态）⇒ 空，不是残留
	meta2 := &CodeMeta{ShareCode: "LEGACY1"}
	enrichMetaFromDB(meta2, &model.FileCode{Code: "LEGACY1", Encrypted: true})
	assert.Empty(t, meta2.WrappedKey)

	// 已有值不被覆盖（与 FileName/FileSize 同款"空才回填"语义）
	meta3 := &CodeMeta{ShareCode: "KEEP01", WrappedKey: "preset"}
	enrichMetaFromDB(meta3, &model.FileCode{Code: "KEEP01", WrappedKey: "other"})
	assert.Equal(t, "preset", meta3.WrappedKey)
}
