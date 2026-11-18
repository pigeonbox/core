package anonymous

// 仅站内（internal_only）分享在**匿名域**必须被拒（core#233 取件环）。
//
// 为什么这条独立：匿名域没有身份能力 ⇒ 仅站内分享不能走匿名取件；
// 且 /anonymous/download 会给「存在/未过期/未管控/无密码」的分享**签发下载
// 令牌** ⇒ 不拦就是令牌旁路（令牌反手解锁 /share/download）。

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/repo/db/model"
)

// TestPeek_InternalOnly_Rejected Peek（/anonymous/download + /anonymous/search 共用）须拒。
func TestPeek_InternalOnly_Rejected(t *testing.T) {
	svc, _, g := newTestService(t)
	ctx := context.Background()
	require.NoError(t, g.Create(&model.FileCode{
		Code: "abcd1234", FilePath: "uploads/x.bin", Size: 10, Text: "a.txt",
		InternalOnly: true,
	}).Error)

	_, _, err := svc.Peek(ctx, "abcd1234")
	require.Error(t, err, "仅站内分享不得在匿名域可查（否则匿名下载面会给它签发下载令牌）")
	var coder interface{ ErrCode() int }
	require.True(t, errors.As(err, &coder), "必须返回 typed error: %v", err)
	assert.Equal(t, errcode.CodeUnauthorized, coder.ErrCode())
}

// TestRetrieve_InternalOnly_Rejected Retrieve（/anonymous/retrieve）须拒。
func TestRetrieve_InternalOnly_Rejected(t *testing.T) {
	svc, _, g := newTestService(t)
	ctx := context.Background()
	require.NoError(t, g.Create(&model.FileCode{
		Code: "abcd1234", FilePath: "uploads/x.bin", Size: 10, Text: "a.txt",
		InternalOnly: true,
	}).Error)

	_, err := svc.Retrieve(ctx, "abcd1234", "", "1.2.3.4")
	require.Error(t, err, "仅站内分享不得被匿名取件")
}

// TestPeek_NonInternalOnly_Allowed 对照：普通分享照旧可查（不得误伤）。
func TestPeek_NonInternalOnly_Allowed(t *testing.T) {
	svc, _, g := newTestService(t)
	ctx := context.Background()
	require.NoError(t, g.Create(&model.FileCode{
		Code: "abcd1234", FilePath: "uploads/x.bin", Size: 10, Text: "a.txt",
	}).Error)

	_, fc, err := svc.Peek(ctx, "abcd1234")
	require.NoError(t, err, "普通分享不受仅站内策略影响")
	require.NotNil(t, fc)
}
