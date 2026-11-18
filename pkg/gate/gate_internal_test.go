package gate

// 仅站内（internal_only）取件闸口的断言（core#233 取件环）。

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/contracts/errcode"
)

func TestCheckInternalOnlyLogin(t *testing.T) {
	// 非仅站内 ⇒ 放行（匿名也不拦）
	require.NoError(t, CheckInternalOnlyLogin(nil, false))

	// 仅站内 + 匿名 ⇒ 拒（typed error ⇒ CodeUnauthorized ⇒ HTTP 401）
	err := CheckInternalOnlyLogin(nil, true)
	require.Error(t, err)
	var coder interface{ ErrCode() int }
	require.True(t, errors.As(err, &coder))
	assert.Equal(t, errcode.CodeUnauthorized, coder.ErrCode())
	assert.Contains(t, err.Error(), "仅限站内取件")

	// 仅站内 + 已登录 ⇒ 放行
	uid := uint(1)
	require.NoError(t, CheckInternalOnlyLogin(&uid, true))
}
