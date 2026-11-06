package middleware

// 5.2.0 会话闸：带 sid 的令牌必须命中「仍然有效」的会话集合。
//
// 负对照（把对应实现改回旧行为即红）：
//   - 删掉 identityFresh 里的会话集合判定 ⇒「已撤销会话必须被拒」用例红
//     （漏洞：踢掉的设备照样能用）；
//   - 把该判定改成「Sessions 为 nil 也拒绝」⇒「无 sid 老令牌仍可用」用例红
//     （漏洞：升级瞬间全员下线）。
//
// 两条必须同时成立，所以两个用例都在：这是「反向兼容」与「真的能踢」之间的
// 那条窄缝，只有单向的守卫会让人以为守住了。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pigeonbox/core/pkg/auth"
)

func withTrustIdentity(t *testing.T, userID uint, rec *IdentityRecord) {
	t.Helper()
	SetIdentityLoader(func(ctx context.Context, id uint) (*IdentityRecord, error) {
		if id != userID {
			return nil, nil
		}
		return rec, nil
	})
	// 缓存是 30s TTL 的包级单例 ⇒ 每个用例先清掉上一个用例留下的快照
	InvalidateIdentity(userID)
	t.Cleanup(func() {
		InvalidateIdentity(userID)
		SetIdentityLoader(nil)
	})
}

func TestIdentityFresh_SessionGate(t *testing.T) {
	const uid uint = 4242
	withTrustIdentity(t, uid, &IdentityRecord{
		Status:   "active",
		Role:     "user",
		Epoch:    3,
		Sessions: map[string]bool{"alive": true},
	})

	// ① 有效会话 ⇒ 通过（否则「登录一次就被踢」这种灾难不会有人漏看）
	assert.True(t, IdentityFresh(context.Background(), &auth.Claims{UserID: uid, Epoch: 3, SessionID: "alive"}))

	// ② 已撤销/未知会话 ⇒ 拒绝（这是「退出此设备」真正生效的地方）
	assert.False(t, IdentityFresh(context.Background(), &auth.Claims{UserID: uid, Epoch: 3, SessionID: "revoked"}),
		"会话不在有效集合里必须拒绝")

	// ③ 无 sid（升级前签发的老令牌）⇒ 沿用纪元判定，仍可用
	assert.True(t, IdentityFresh(context.Background(), &auth.Claims{UserID: uid, Epoch: 3}),
		"无 sid 的老令牌不能因会话闸被踢（升级瞬间全员下线是更严重的故障）")

	// ④ 纪元不匹配 ⇒ 依然拒绝（会话闸不能把纪元闸顶掉）
	assert.False(t, IdentityFresh(context.Background(), &auth.Claims{UserID: uid, Epoch: 2, SessionID: "alive"}))
}

func TestIdentityFresh_SessionsNilSkipsGate(t *testing.T) {
	const uid uint = 4343
	// Sessions 为 nil = 装配点未提供（轻量环境）⇒ 跳过会话校验，只走纪元判定
	withTrustIdentity(t, uid, &IdentityRecord{Status: "active", Role: "user", Epoch: 1})

	assert.True(t, IdentityFresh(context.Background(), &auth.Claims{UserID: uid, Epoch: 1, SessionID: "whatever"}),
		"未装配会话集合时不得把所有人判成已撤销")
}

func TestIdentityFresh_EmptySessionsRejectsAll(t *testing.T) {
	const uid uint = 4444
	// 空 map（非 nil）= 该用户零有效会话 ⇒ 带 sid 的令牌一律拒绝
	withTrustIdentity(t, uid, &IdentityRecord{Status: "active", Role: "user", Epoch: 1, Sessions: map[string]bool{}})

	assert.False(t, IdentityFresh(context.Background(), &auth.Claims{UserID: uid, Epoch: 1, SessionID: "any"}),
		"零有效会话 ⇒ 带 sid 的令牌必须拒绝")
	assert.True(t, IdentityFresh(context.Background(), &auth.Claims{UserID: uid, Epoch: 1}),
		"无 sid 令牌只受纪元约束")
}
