package share

// 仅站内（internal_only）目录策略的断言（core#233）。
//
// 语义两环都约束：
//   ① 生成环：命中 internal_only 目录的分享**不铸 6 位公网取件码**；
//   ② 取件环：该分享的内容交付面**强制登录**（匿名拒、登录放行）。
//
// 判定基准：**建行时定型**（model.FileCode.InternalOnly）⇒ 取件面读行上标记，
// 不查当前配置、不做文件系统解析。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
)

// mockMinter 取件码铸造替身（环一断言用：仅站内分享不得铸码）。
type mockMinter struct {
	calls int
	code  string
}

func (m *mockMinter) MintForShare(_ context.Context, _, _ string, _ int64, _ bool, _ *time.Time) (string, error) {
	m.calls++
	return m.code, nil
}

// TestImportLocalFile_InternalOnly_NoPublicPickupCode 环一：仅站内目录的分享不铸公网码。
func TestImportLocalFile_InternalOnly_NoPublicPickupCode(t *testing.T) {
	root := t.TempDir()
	p := writeLocalFile(t, root, "a.txt", 100)
	withDirectoryPolicy(t, root, 0, conf.ShareModeInternalOnly)

	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	minter := &mockMinter{code: "ABC234"}
	svc.SetPickupMinter(minter)

	resp, err := svc.ImportLocalFile(context.Background(), ImportLocalOpts{AbsPath: p})
	require.NoError(t, err, "internal_only 不拦创建（它约束的是「仅站内可取」，不是「不许分享」）")

	assert.Equal(t, 0, minter.calls, "仅站内分享不得铸造公网取件码（6 位码是公网凭证）")
	assert.Empty(t, resp.PickupCode, "响应里不得出现公网取件码")
	assert.NotEmpty(t, resp.Code, "8 位分享码照旧存在（站内入口）")

	// 标记落到行上（建行时定型 ⇒ 取件面靠它判定）
	var fc model.FileCode
	require.NoError(t, gormDB.Where("file_path <> ''").First(&fc).Error)
	assert.True(t, fc.InternalOnly, "仅站内标记必须落到行上")
}

// TestImportLocalFile_Allow_StillMintsPickupCode 对照：非仅站内的分享照旧铸码。
func TestImportLocalFile_Allow_StillMintsPickupCode(t *testing.T) {
	root := t.TempDir()
	p := writeLocalFile(t, root, "a.txt", 100)
	withDirectoryPolicy(t, root, 0, conf.ShareModeAllow)

	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	minter := &mockMinter{code: "ABC234"}
	svc.SetPickupMinter(minter)

	resp, err := svc.ImportLocalFile(context.Background(), ImportLocalOpts{AbsPath: p})
	require.NoError(t, err)
	assert.Equal(t, 1, minter.calls, "非仅站内的分享照旧铸 6 位取件码")
	assert.Equal(t, "ABC234", resp.PickupCode)

	var fc model.FileCode
	require.NoError(t, gormDB.Where("file_path <> ''").First(&fc).Error)
	assert.False(t, fc.InternalOnly, "非仅站内目录的行不得带标记")
}

// TestGetFileByCodeFor_InternalOnly_RequiresLogin 环二：匿名拒、登录放行。
func TestGetFileByCodeFor_InternalOnly_RequiresLogin(t *testing.T) {
	root := t.TempDir()
	p := writeLocalFile(t, root, "a.txt", 100)
	withDirectoryPolicy(t, root, 0, conf.ShareModeInternalOnly)

	svc, _, _, _, _ := newTestServiceWithDB(t)
	svc.SetPickupMinter(&mockMinter{code: "ABC234"})
	resp, err := svc.ImportLocalFile(context.Background(), ImportLocalOpts{AbsPath: p})
	require.NoError(t, err)

	// 匿名 ⇒ 拒（typed error ⇒ CodeUnauthorized ⇒ HTTP 401）
	_, err = svc.GetFileByCodeFor(context.Background(), resp.Code, nil)
	require.Error(t, err, "仅站内分享的匿名取件必须被拒")
	var coder interface{ ErrCode() int }
	require.True(t, errors.As(err, &coder), "必须返回 typed error: %v", err)
	assert.Equal(t, errcode.CodeUnauthorized, coder.ErrCode(), "业务码必须是 10002（复用登录要求口径）")
	assert.Contains(t, err.Error(), "仅限站内取件", "文案要说清是「仅站内」")

	// 已登录 ⇒ 放行（站内专享：登录用户正常取件）
	uid := uint(7)
	got, err := svc.GetFileByCodeFor(context.Background(), resp.Code, &uid)
	require.NoError(t, err, "已登录用户取仅站内分享必须放行")
	assert.Equal(t, resp.Code, got.Code)
}

// TestGetFileByCodeFor_NonInternalOnly_AnonymousAllowed 对照：普通分享匿名照旧可取。
func TestGetFileByCodeFor_NonInternalOnly_AnonymousAllowed(t *testing.T) {
	root := t.TempDir()
	p := writeLocalFile(t, root, "a.txt", 100)
	withDirectoryPolicy(t, root, 0, conf.ShareModeAllow)

	svc, _, _, _, _ := newTestServiceWithDB(t)
	svc.SetPickupMinter(&mockMinter{code: "ABC234"})
	resp, err := svc.ImportLocalFile(context.Background(), ImportLocalOpts{AbsPath: p})
	require.NoError(t, err)

	got, err := svc.GetFileByCodeFor(context.Background(), resp.Code, nil)
	require.NoError(t, err, "普通分享的匿名取件不受仅站内策略影响")
	assert.Equal(t, resp.Code, got.Code)
}
