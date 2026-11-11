package chunk

import (
	"errors"
	"testing"

	"github.com/pigeonbox/contracts/errcode"

	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/storage"
)

// TestValidateChunkBytes（core#140 正判据）：恰好上限放行、+1 报 typed
// FileTooLargeError（ErrCode()==CodeTooLarge ⇒ handler 透传 413）。
//
// 负对照（改错转红）：去掉/放宽校验 ⇒ 超限用例转红。
func TestValidateChunkBytes(t *testing.T) {
	if err := ValidateChunkBytes(storage.MaxChunkBytes); err != nil {
		t.Fatalf("恰好上限应放行: %v", err)
	}
	err := ValidateChunkBytes(storage.MaxChunkBytes + 1)
	if err == nil {
		t.Fatal("超限必须报错")
	}
	var fe *utils.FileTooLargeError
	if !errors.As(err, &fe) {
		t.Fatalf("应为 *utils.FileTooLargeError（typed error，供 413 透传）: %v", err)
	}
	if fe.ErrCode() != errcode.CodeTooLarge {
		t.Fatalf("业务码应为 CodeTooLarge: got=%d", fe.ErrCode())
	}
}
