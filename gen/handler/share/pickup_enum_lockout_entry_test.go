// 取件枚举「四入口统一锁」的 search/download 逐点负向（core#183 残差 S03a/#175）。
//
// 背景：metadata 入口已有「10 记账 → 第 11 次 429」的直接负向
// （transport/http/handler/share_metadata_test.go），search/download/preview
// 三入口缺各自的负向 ⇒「四入口统一锁」没有逐点钉：某个入口的锁定一旦失效，
// 只有恰好撞上其它入口的用例才可能发现 ⇒ 全绿假象。
//
// 两入口的预检位置不同，负向形态也不同（这正是要逐点钉的原因）：
//   - download：解析**前**预检（authorizeDownload）⇒ 记满后连不存在的码也 429；
//   - search：解析**后**预检（GetShare 先拿规范码）⇒ 不存在的码恒 404、
//     到不了预检，必须用**存在的码**观察 429（枚举维度按 IP、换码也命中）。
//
// 记账一律用**互不相同的不存在码** ⇒ (IP,码) 维度每码各 1 次、永远到不了阈值，
// 唯一能累计到阈值的只有按 IP 的枚举维度 ⇒ 断言命中的正是「枚举锁」本身。
//
// 断言必须能变红（两条判据，均已实测）：
//  1. 撤掉按 IP 的**枚举维度记账**（pkg/gate.RecordPickupFailure 的枚举段）⇒
//     桶永不锁 ⇒ search 第 11 次落回 200、download 落回 404 ⇒ 两用例同时转红；
//  2. download 单独钉住其解析**前**预检：撤掉该预检（记账保留）⇒ 第 11 次落回
//     10004/404——不存在的码到不了 CheckPickup，429 只可能来自该预检 ⇒ 转红。
//     （search 的 429 对存在的码由预检 + CheckPickup 双重兜底，故单撤预检不红；
//     它钉的是「入口在枚举锁定后拒绝取件」这一层。）
package share

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// TestGetShare_EnumLockoutAfterTenMisses search 入口（GET /share/select/）。
func TestGetShare_EnumLockoutAfterTenMisses(t *testing.T) {
	g := newChargeTestEnv(t, true)
	withPickupLockout(t, 10)
	// 存在的码：本入口预检在解析成功之后，第 11 次要用它观察 429
	insertFileCode(t, g, &model.FileCode{
		Code: "ENUMSEL1", Text: "visible", FilePath: "", ExpiredCount: 3,
	})

	// 前 10 次：互不相同的不存在码 ⇒ 业务 404（入枚举桶），不得提前 429
	for i := 0; i < 10; i++ {
		status, body := performGet(t, "/share/select/", fmt.Sprintf("code=NOSE%04d", i), hostHeader())
		require.Equal(t, 404, status, "第 %d 次不存在码应为业务 404（记账中）: %s", i+1, body)
	}

	// 第 11 次：按 IP 的枚举维度已锁 ⇒ **存在的码**也必须 429
	//（红判据：撤掉枚举维度记账 ⇒ 桶不锁 ⇒ 此处落回 200）
	status, body := performGet(t, "/share/select/", "code=ENUMSEL1", hostHeader())
	require.Equal(t, 429, status, "枚举锁定后 search 入口必须 429: %s", body)
	require.Contains(t, body, "尝试过于频繁", "429 必须带可判文案: %s", body)
}

// TestDownload_EnumLockoutAfterTenMisses download 入口（GET /share/download）。
func TestDownload_EnumLockoutAfterTenMisses(t *testing.T) {
	newChargeTestEnv(t, true)
	withPickupLockout(t, 10)

	// 前 10 次：互不相同的不存在码 ⇒ 404（入枚举桶）
	for i := 0; i < 10; i++ {
		status, body := performDownloadPath(t, fmt.Sprintf("code=NODN%04d", i))
		require.Equal(t, 404, status, "第 %d 次不存在码应为 404（记账中）: %s", i+1, body)
	}

	// 第 11 次：本入口解析前预检命中枚举维度 ⇒ 连不存在的码也 429
	//（不存在的码到不了 CheckPickup ⇒ 429 只可能来自该预检，已实测撤掉即转红）
	status, body := performDownloadPath(t, "code=NODN9999")
	require.Equal(t, 429, status, "枚举锁定后 download 入口必须 429: %s", body)
	require.Contains(t, body, "尝试过于频繁", "429 必须带可判文案: %s", body)
}
