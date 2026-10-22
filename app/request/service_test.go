package request

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateRejectsCountExpireStyle 钉住「投递码不接受『按取件次数』过期」。
//
// 2026-10-22 审计：该域的过期是**按时间**算的（取件次数恒为无限
// ExpiredCount=defaultRecvCount=-1），但 count 能通过 CheckExpireStyleAllowed
// 的白名单校验 ⇒ 用户传 count 拿到的是「30 天 + 无限次」，与「按次数」的
// 期望**静默错配**。现在响亮拒绝。
//
// 断言放在碰 DB 之前 ⇒ 无需真实数据库。
func TestCreateRejectsCountExpireStyle(t *testing.T) {
	s := &Service{} // 计数校验在任何 DB 访问之前，无需注入 repo

	_, err := s.Create(context.Background(), 1, CreateReq{
		Title:       "t",
		ExpireStyle: "count",
		ExpireValue: 3,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "按取件次数")
	// 对照组（按时间的样式会继续往下走到 DB）需要真实库，不在此断言。
}
