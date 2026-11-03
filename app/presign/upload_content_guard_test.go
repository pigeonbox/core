// presign 通道「内容拒绝要早、业务码要对」守卫（2026-11-03，真机缺陷驱动）。
//
// 缺陷实录：同事传 ZCode 的 .AppImage（＝ELF）——
//   ① 魔数误杀（.appimage 是诚实的可执行扩展名，应豁免）；
//   ② 误杀发生在 **Complete** ⇒ 1.35GB 传完才报错，带宽全白费；
//   ③ 报错业务码塌成 10008「内部错误」（应为 30011 文件类型不允许）。
//
// 断言必须能变红（负对照）：
//   - 清空 magicCheckSkipExts ⇒ TestPresignFlow_AppImage_Succeeds 红（复现误杀）；
//   - 去掉 UploadDirect 的预检 ⇒ …RejectedBeforeWrite 红（对象已被写入）；
//   - 去掉 Complete 的 typed error 透传（default 塌 10008）⇒
//     TestComplete_ContentRecheck_CarriesBusinessCode 在 handler 层变红。
package presign

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/pkg/utils"
)

// fakeStorage 内存存储桩：同时实现 StorageWriter + headReader（能力探测式
// 接口，方法集满足即可），记录写入用于「早拒绝不落对象」断言。
type fakeStorage struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newFakeStorage() *fakeStorage { return &fakeStorage{files: map[string][]byte{}} }

func (f *fakeStorage) SaveBytes(_ context.Context, p string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	f.files[p] = cp
	return nil
}

func (f *fakeStorage) SaveStream(_ context.Context, p string, r io.Reader, _ int64) (int64, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	return int64(len(data)), f.SaveBytes(context.Background(), p, data)
}

func (f *fakeStorage) DeleteFile(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, p)
	return nil
}

func (f *fakeStorage) GetFileReader(_ context.Context, p string) (io.ReadCloser, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[p]
	if !ok {
		return nil, 0, errors.New("object not found")
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

func (f *fakeStorage) savedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.files)
}

var fakeELF = []byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00}

// TestUploadDirect_RejectsDisguisedExecutable_BeforeWrite 伪装文件（cat.jpg 实为
// ELF）必须在 **PUT 写入前** 被拒：不落对象、带 30011 业务码。
// 负对照：去掉 UploadDirect 的 Peek 预检 ⇒ 对象已写入（savedCount>0）⇒ 红。
func TestUploadDirect_RejectsDisguisedExecutable_BeforeWrite(t *testing.T) {
	svc, _ := newTestService(t)
	st := newFakeStorage()
	svc.SetStorage(st)
	svc.SetShareService(&mockShareService{returnCode: "code"})

	ctx := context.Background()
	body := append(fakeELF, bytes.Repeat([]byte("x"), 64)...)
	initRes, err := svc.Init(ctx, InitMeta{FileName: "cat.jpg", FileSize: int64(len(body))})
	require.NoError(t, err)

	err = svc.UploadDirect(ctx, initRes.UploadID, initRes.Token, bytes.NewReader(body), int64(len(body)))
	var mme *utils.MagicMismatchError
	require.True(t, errors.As(err, &mme), "伪装文件应在写入前被拒，实际: %v", err)
	// typed error 契约：30011（不是 10008）
	var coder interface{ ErrCode() int }
	require.True(t, errors.As(err, &coder))
	assert.Equal(t, errcode.CodeFileTypeDenied, coder.ErrCode())
	assert.Equal(t, 0, st.savedCount(), "早拒绝必须发生在写入前——对象不得落存储")
}

// TestPresignFlow_AppImage_Succeeds 真机误杀场景的正片：.AppImage（ELF）
// init → PUT → Complete 全流程必须成功。
// 负对照：清空 magicCheckSkipExts（旧实现）⇒ Complete 报「内容与扩展名不符」⇒ 红。
func TestPresignFlow_AppImage_Succeeds(t *testing.T) {
	svc, _ := newTestService(t)
	st := newFakeStorage()
	svc.SetStorage(st)
	svc.SetShareService(&mockShareService{returnCode: "SHARE-APPIMAGE"})

	ctx := context.Background()
	body := append(fakeELF, bytes.Repeat([]byte{0x00}, 128)...)
	initRes, err := svc.Init(ctx, InitMeta{FileName: "ZCode-3.15.3-linux-x64.AppImage", FileSize: int64(len(body))})
	require.NoError(t, err)

	require.NoError(t, svc.UploadDirect(ctx, initRes.UploadID, initRes.Token, bytes.NewReader(body), int64(len(body))),
		".AppImage 是诚实的 ELF 载体，PUT 不应被拒")

	res, err := svc.Complete(ctx, initRes.UploadID, initRes.Token, "127.0.0.1")
	require.NoError(t, err, ".AppImage 全流程必须成功（旧实现在此报『文件内容与扩展名不符』）")
	assert.Equal(t, "SHARE-APPIMAGE", res.ShareCode)
}

// TestComplete_ContentRecheck_CarriesBusinessCode Complete 期兜底复检（对象
// 经旁路落盘、绕过 PUT 预检时仍拦得住）+ typed error 契约：错误必须带 30011。
func TestComplete_ContentRecheck_CarriesBusinessCode(t *testing.T) {
	svc, _ := newTestService(t)
	st := newFakeStorage()
	svc.SetStorage(st)
	mock := &mockShareService{returnCode: "code"}
	svc.SetShareService(mock)

	ctx := context.Background()
	initRes, err := svc.Init(ctx, InitMeta{FileName: "cat.jpg", FileSize: int64(len(fakeELF))})
	require.NoError(t, err)

	// 旁路直传（模拟对象已被写入的存量通道）：直接落存储
	require.NoError(t, st.SaveBytes(ctx, initRes.ObjectKey, fakeELF))

	_, err = svc.Complete(ctx, initRes.UploadID, initRes.Token, "127.0.0.1")
	var mme *utils.MagicMismatchError
	require.True(t, errors.As(err, &mme), "Complete 兜底复检应拒绝伪装文件，实际: %v", err)
	var coder interface{ ErrCode() int }
	require.True(t, errors.As(err, &coder))
	assert.Equal(t, errcode.CodeFileTypeDenied, coder.ErrCode())
	assert.Equal(t, 0, mock.called, "被拒内容不得建分享")
	assert.Equal(t, 0, st.savedCount(), "被拒对象应被清理")
}
