package bootstrap

// 分享到期提醒（5.5.0）的判据用例。
//
// 这里刻意**不用**真的通知服务：注入一个 stub 就能把「发了没发」「发了几次」
// 变成可断言的整数，也能让「发送失败」这个分支被真实触发（真实渠道失败要造
// 网络错误，既慢又不稳）。
//
// 覆盖的判据（每条都对应一处会让用户多收/漏收提醒的缺陷）：
//   C1 窗口内 + 寿命长于提前量  ⇒ 发一条、标记落库
//   C2 再跑一轮               ⇒ 不重复（幂等）
//   C3 开关关闭               ⇒ 一条都不发
//   C4 匿名分享（无 owner）    ⇒ 不发（没有收件人）
//   C5 寿命 ≤ 提前量           ⇒ 不发（刚发出就说「24 小时后过期」是噪音）
//   C6 次数已耗尽 / 被禁用      ⇒ 不发（这两种分享本来就不能取件）
//   C7 延期后                 ⇒ 标记被清 ⇒ 新一轮到期还能再提醒一次
//   C8 发送失败               ⇒ 标记回滚，下轮重试成功（不静默丢）

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// stubExpiryNotifier 记录每次发送；failNext 为 true 时下一次发送失败。
type stubExpiryNotifier struct {
	calls    []uint // 收件人 user id
	titles   []string
	failNext bool
}

func (s *stubExpiryNotifier) CreateForUserSimple(_ context.Context, userID uint, title, _ string, _, _ string) error {
	if s.failNext {
		s.failNext = false
		return errors.New("渠道不可用")
	}
	s.calls = append(s.calls, userID)
	s.titles = append(s.titles, title)
	return nil
}

func newExpiryReminderTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1) // glebarez/sqlite :memory: 多连接各为独立库，钉死单连接防 flake
	return g
}

// seedExpiryShare 造一条分享：
//
//	created 到现在多久建的，expire 到现在还有多久过期（负值 = 已过期）。
func seedExpiryShare(t *testing.T, g *gorm.DB, code string, userID *uint, now time.Time, createdAgo, expireIn time.Duration, _ any) *model.FileCode {
	t.Helper()
	exp := now.Add(expireIn)
	f := &model.FileCode{
		Code:         code,
		Status:       model.StatusNormal,
		ExpiredCount: -1,
		UserID:       userID,
		ExpiredAt:    &exp,
	}
	f.CreatedAt = now.Add(-createdAgo)
	require.NoError(t, g.Create(f).Error)
	return f
}

func enabledCfg() conf.ExpiryReminderConfig {
	return conf.ExpiryReminderConfig{Enabled: true, AdvanceHours: 24, IntervalMinutes: 60}
}

func uintPtr(v uint) *uint { return &v }

func TestExpiryReminderSendsOnceAndIsIdempotent(t *testing.T) {
	g := newExpiryReminderTestDB(t)
	repo := dao.NewFileCodeRepository(g)
	notifier := &stubExpiryNotifier{}
	now := time.Now()

	// 寿命 7 天、还剩 20 小时到期（落在 24 小时窗口内，且寿命 > 窗口）
	seedExpiryShare(t, g, "AA11", uintPtr(7), now, 7*24*time.Hour-20*time.Hour, 20*time.Hour, nil)

	// C1：一轮发出 1 条，收件人是 owner
	sent, err := runExpiryReminderRound(context.Background(), repo, notifier, enabledCfg(), now)
	require.NoError(t, err)
	require.Equal(t, 1, sent)
	require.Len(t, notifier.calls, 1)
	assert.Equal(t, uint(7), notifier.calls[0])

	// 标记落库
	var got model.FileCode
	require.NoError(t, g.Where("code = ?", "AA11").First(&got).Error)
	require.NotNil(t, got.ExpiryRemindedAt, "提醒后必须写去重标记")

	// C2：同一时刻再跑一轮，一条都不发
	sent, err = runExpiryReminderRound(context.Background(), repo, notifier, enabledCfg(), now.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 0, sent, "同一分享不得重复提醒")
	assert.Len(t, notifier.calls, 1)
}

func TestExpiryReminderSkipsWhenDisabled(t *testing.T) {
	g := newExpiryReminderTestDB(t)
	repo := dao.NewFileCodeRepository(g)
	notifier := &stubExpiryNotifier{}
	now := time.Now()
	seedExpiryShare(t, g, "BB22", uintPtr(7), now, 6*24*time.Hour, 20*time.Hour, nil)

	cfg := enabledCfg()
	cfg.Enabled = false
	sent, err := runExpiryReminderRound(context.Background(), repo, notifier, cfg, now)
	require.NoError(t, err)
	assert.Equal(t, 0, sent, "开关关闭时不得发送")
	assert.Empty(t, notifier.calls)
}

func TestExpiryReminderSkipsAnonymousAndShortLived(t *testing.T) {
	g := newExpiryReminderTestDB(t)
	repo := dao.NewFileCodeRepository(g)
	notifier := &stubExpiryNotifier{}
	now := time.Now()

	// C4：匿名分享（user_id 为空）——没有收件人
	seedExpiryShare(t, g, "CC33", nil, now, 6*24*time.Hour, 20*time.Hour, nil)
	// C5：寿命 2 小时、还剩 1 小时到期 ⇒ 寿命 ≤ 提前量，不该提醒
	seedExpiryShare(t, g, "DD44", uintPtr(7), now, time.Hour, time.Hour, nil)

	sent, err := runExpiryReminderRound(context.Background(), repo, notifier, enabledCfg(), now)
	require.NoError(t, err)
	assert.Equal(t, 0, sent, "匿名与短命分享都不在提醒范围")
	assert.Empty(t, notifier.calls)
}

func TestExpiryReminderSkipsExhaustedAndBlocked(t *testing.T) {
	g := newExpiryReminderTestDB(t)
	repo := dao.NewFileCodeRepository(g)
	notifier := &stubExpiryNotifier{}
	now := time.Now()

	exhausted := seedExpiryShare(t, g, "EE55", uintPtr(7), now, 6*24*time.Hour, 20*time.Hour, nil)
	exhausted.ExpiredCount = 0
	require.NoError(t, g.Model(exhausted).Update("expired_count", 0).Error)

	blocked := seedExpiryShare(t, g, "FF66", uintPtr(7), now, 6*24*time.Hour, 20*time.Hour, nil)
	blocked.Status = model.StatusBlocked
	require.NoError(t, g.Model(blocked).Update("status", model.StatusBlocked).Error)

	sent, err := runExpiryReminderRound(context.Background(), repo, notifier, enabledCfg(), now)
	require.NoError(t, err)
	assert.Equal(t, 0, sent, "次数耗尽的分享提醒「请延长」是误导；被禁用的分享提醒会泄露治理动作")
	assert.Empty(t, notifier.calls)
}

func TestExpiryReminderReArmsAfterExtend(t *testing.T) {
	g := newExpiryReminderTestDB(t)
	repo := dao.NewFileCodeRepository(g)
	notifier := &stubExpiryNotifier{}
	now := time.Now()

	seedExpiryShare(t, g, "GG77", uintPtr(7), now, 6*24*time.Hour, 20*time.Hour, nil)

	sent, err := runExpiryReminderRound(context.Background(), repo, notifier, enabledCfg(), now)
	require.NoError(t, err)
	require.Equal(t, 1, sent)

	// owner 延期（+7 天）⇒ 标记应被清空
	newExp := now.Add(7 * 24 * time.Hour)
	n, err := repo.BatchExtendByCodes(context.Background(), 7, []string{"GG77"}, &newExp)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	var got model.FileCode
	require.NoError(t, g.Where("code = ?", "GG77").First(&got).Error)
	require.Nil(t, got.ExpiryRemindedAt, "延期后标记必须清空（新一轮到期还要能提醒）")

	// 新到期日进入窗口 ⇒ 再提醒一次
	later := newExp.Add(-20 * time.Hour)
	sent, err = runExpiryReminderRound(context.Background(), repo, notifier, enabledCfg(), later)
	require.NoError(t, err)
	assert.Equal(t, 1, sent, "延期后的新一轮到期应当再提醒一次")
	assert.Len(t, notifier.calls, 2)
}

func TestExpiryReminderRollsBackMarkOnSendFailure(t *testing.T) {
	g := newExpiryReminderTestDB(t)
	repo := dao.NewFileCodeRepository(g)
	notifier := &stubExpiryNotifier{failNext: true}
	now := time.Now()

	seedExpiryShare(t, g, "HH88", uintPtr(7), now, 6*24*time.Hour, 20*time.Hour, nil)

	sent, err := runExpiryReminderRound(context.Background(), repo, notifier, enabledCfg(), now)
	require.NoError(t, err)
	require.Equal(t, 0, sent, "发送失败不计入成功数")

	var got model.FileCode
	require.NoError(t, g.Where("code = ?", "HH88").First(&got).Error)
	require.Nil(t, got.ExpiryRemindedAt, "发送失败必须回滚标记，否则这条提醒永久丢失")

	// 下一轮重试成功
	sent, err = runExpiryReminderRound(context.Background(), repo, notifier, enabledCfg(), now.Add(2*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 1, sent, "回滚后下一轮必须能重新发出")
	assert.Len(t, notifier.calls, 1)
}

// TestMarkExpiryRemindedArbitrates 抢标记必须**只有第一个**成功。
//
// 这条单独测：端到端的「跑两轮」并不能证明仲裁有效——候选查询本身就过滤了
// 已提醒的行，所以第二个副本在查询阶段就被滤掉了，无条件 UPDATE 也能通过
// 端到端用例。真正的多副本场景是「两个副本**同时**查询到同一批候选」，
// 仲裁只发生在 UPDATE 这一步 ⇒ 直接测这一步。
func TestMarkExpiryRemindedArbitrates(t *testing.T) {
	g := newExpiryReminderTestDB(t)
	repo := dao.NewFileCodeRepository(g)
	now := time.Now()
	f := seedExpiryShare(t, g, "II99", uintPtr(7), now, 6*24*time.Hour, 20*time.Hour, nil)

	won, err := repo.MarkExpiryReminded(context.Background(), f.ID, now)
	require.NoError(t, err)
	require.True(t, won, "第一个副本应抢到标记")

	won, err = repo.MarkExpiryReminded(context.Background(), f.ID, now.Add(time.Second))
	require.NoError(t, err)
	assert.False(t, won, "第二个副本不得抢到（否则会重复发一条提醒）")
}

func TestClampExpiryReminderConfig(t *testing.T) {
	cases := []struct {
		name        string
		in          conf.ExpiryReminderConfig
		wantAdvance time.Duration
		wantInter   time.Duration
	}{
		{"零值取默认", conf.ExpiryReminderConfig{}, 24 * time.Hour, 60 * time.Minute},
		{"下限收敛", conf.ExpiryReminderConfig{AdvanceHours: -5, IntervalMinutes: -1}, time.Hour, time.Minute},
		{"上限收敛", conf.ExpiryReminderConfig{AdvanceHours: 100000, IntervalMinutes: 100000}, 720 * time.Hour, 1440 * time.Minute},
		{"正常值原样", conf.ExpiryReminderConfig{AdvanceHours: 48, IntervalMinutes: 15}, 48 * time.Hour, 15 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			adv, itv := clampExpiryReminderConfig(c.in)
			assert.Equal(t, c.wantAdvance, adv)
			assert.Equal(t, c.wantInter, itv)
		})
	}
}
