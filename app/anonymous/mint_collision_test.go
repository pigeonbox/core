package anonymous

import (
	"context"
	"testing"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 取件码铸造的**跨列避让**（2026-10-23 随「取件码也能当页面链接」新能力引入）。
//
// 背景：分享域的「按码取分享」现在同时认 8 位分享码（`code` 列）与 6 位取件码
// （`pickup_code` 列），且**分享码优先**。若某个取件码的字符串同时被 `code`
// 列占用（自定义分享码允许 3-32 位，能是 6 位），那个取件码就会打开**别人**
// 的分享（错配）。⇒ 铸造取件码时必须确认该串在 `code` 列也未被占用。
//
// 本测试用**属性断言**（不依赖随机碰撞）：每次铸造出的取件码，在 `code` 列
// 都必须是空闲的。
func TestMintForShare_PickupCodeAvoidsShareCodeColumn(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	expireAt := time.Now().Add(time.Hour)

	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code:         "shareabc",
		FilePath:     "uploads/x/shareabc.bin",
		ExpiredCount: -1,
	}))

	for i := 0; i < 20; i++ {
		pickup, err := svc.MintForShare(ctx, "shareabc", "demo.txt", 128, false, &expireAt)
		require.NoError(t, err)
		require.Len(t, pickup, codeLength)

		// 跨列避让：该取件码不得与任何 code 列占用冲突
		taken, err := svc.fileCodeRepo.CheckCodeExists(ctx, pickup, 0)
		require.NoError(t, err)
		assert.False(t, taken, "取件码 %s 撞上了 code 列 ⇒ 按码取分享会打开别人的分享", pickup)
	}
}
