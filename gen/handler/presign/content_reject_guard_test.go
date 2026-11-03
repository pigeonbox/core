// presign handler「内容拒绝 ⇒ 30011/415」映射守卫（2026-11-03）。
//
// 真机实录：同事传 .AppImage 被魔数误杀时，Complete 响应是
// {"code":10008,"message":"文件内容与扩展名不符…"} —— message 清晰但业务码塌成
// 10008「内部错误」（default 分支把非预设错误一律按内部错误处理），
// 且 HTTP 状态是 500 语义，客户端无从区分「内容不合格」与「服务端坏了」。
//
// 修法：default 分支改 resp.NewTypedError —— typed error 按 ErrCode 透传
// （此处 30011 ⇒ HTTP 415），未实现 ErrCode 的仍按 10008 兜底（行为不回退）。
//
// 断言必须能变红（负对照）：把 default 改回 NewErrorWithMessage(CodeInternal) ⇒
// 本用例红（code 10008）。
package presign

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/contracts/errcode"
	presignapp "github.com/pigeonbox/core/app/presign"
)

// fakeHeadStorage 只读桩：对象已存在（旁路落盘场景），供 Complete 的
// 存在性核实（3.5/3.6）与魔数兜底复检（3.8）读取。
type fakeHeadStorage struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (f *fakeHeadStorage) SaveBytes(_ context.Context, p string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.files == nil {
		f.files = map[string][]byte{}
	}
	f.files[p] = data
	return nil
}

func (f *fakeHeadStorage) DeleteFile(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, p)
	return nil
}

func (f *fakeHeadStorage) GetFileReader(_ context.Context, p string) (io.ReadCloser, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[p]
	if !ok {
		return nil, 0, errors.New("object not found")
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

// TestComplete_DisguisedContent_415_FileTypeDenied 对象带可执行魔数伪装
// （cat.jpg 实为 ELF）⇒ Complete 复检拒绝，HTTP 415 + 业务码 30011。
func TestComplete_DisguisedContent_415_FileTypeDenied(t *testing.T) {
	gw := &stubShareGateway{}
	svc := newHandlerEnv(t, gw)
	st := &fakeHeadStorage{}
	svc.SetStorage(st)

	res, err := svc.Init(context.Background(), presignapp.InitMeta{
		FileName: "cat.jpg", FileSize: 8,
	})
	require.NoError(t, err)
	elf := []byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00}
	require.NoError(t, st.SaveBytes(context.Background(), res.ObjectKey, elf))

	code, body := postComplete(t, `{"upload_id":"`+res.UploadID+`","token":"`+res.Token+`"}`)
	assert.Equal(t, http.StatusUnsupportedMediaType, code,
		"内容/类型拒绝应是 415（不是 500 语义的「内部错误」）：%v", body)
	assert.Equal(t, float64(errcode.CodeFileTypeDenied), body["code"],
		"业务码应为 30011 文件类型不允许（不是 10008 内部错误）：%v", body)
	assert.Contains(t, strings.ToLower(body["message"].(string)), "扩展名",
		"message 应保留后端清晰文案（文件内容与扩展名不符）")
}
