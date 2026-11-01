package request

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
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

// =====================================================================
// 寄件码域的行为断言（2026-10-25 审计 A6：该域覆盖率极低 ⇒ 约束无人守护）
// =====================================================================

// stubShareGateway 记录 CreateFromFileEntries 的入参（验证投递落成分享的语义）。
type stubShareGateway struct {
	lastReq *ShareCreateRequest
	code    string
}

func (s *stubShareGateway) CreateFromFileEntries(_ context.Context, req *ShareCreateRequest) (*ShareResult, error) {
	s.lastReq = req
	return &ShareResult{Code: s.code, PickupCode: "PICK01"}, nil
}

// newReqTestService 构造寄件码服务（内存 sqlite + 桩网关）。
func newReqTestService(t *testing.T) (*Service, *stubShareGateway) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileRequest{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	gw := &stubShareGateway{code: "SHARE01"}
	return NewService(g, gw, nil), gw
}

// TestSubmit_CreatedShareIsUnlimitedAndOwnedByOwner 访客投递落成的分享：
//
//	· **按时间过期**（本通道不接受「按取件次数」样式 ⇒ 取件次数恒为无限
//	  ExpiredCount=-1；见 TestCreateRejectsCountExpireStyle）；
//	· 归属**投递链接的主人**（不是访客）。
//
// 这条把「投递通道的取件次数语义」钉死：它是「取件 N 次」承诺的**例外**
// （时间制 ⇒ 无限次），必须显式记录，否则将来有人改成有限次会静默改变语义。
func TestSubmit_CreatedShareIsUnlimitedAndOwnedByOwner(t *testing.T) {
	svc, gw := newReqTestService(t)
	ctx := context.Background()

	created, err := svc.Create(ctx, 42, CreateReq{
		Title: "收件", MaxFiles: 3, MaxBytes: 1024, ExpireValue: 1, ExpireStyle: "day",
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.Token)

	_, err = svc.Submit(ctx, created.Token, []storage.StoredFileEntry{
		{RelPath: "uploads/a.bin", FileName: "a.bin", Size: 10},
	}, "9.9.9.9")
	require.NoError(t, err)

	require.NotNil(t, gw.lastReq)
	assert.Equal(t, -1, gw.lastReq.ExpiredCount,
		"投递通道按时间过期 ⇒ 落成分享的取件次数恒为无限（-1）；改成有限次会静默改变语义")
	assert.Equal(t, uint(42), gw.lastReq.OwnerID, "分享应归属投递链接的主人")
	assert.NotNil(t, gw.lastReq.ExpiredAt, "落成分享必须带时间过期点")
}

// TestSubmit_RejectsOversizedAndUnknownToken 投递约束：空投递 / 文件数超限 /
// 总大小超限 / 未知 token ⇒ 都要明确拒绝。
func TestSubmit_RejectsOversizedAndUnknownToken(t *testing.T) {
	svc, _ := newReqTestService(t)
	ctx := context.Background()

	created, err := svc.Create(ctx, 7, CreateReq{
		Title: "收件", MaxFiles: 1, MaxBytes: 100, ExpireValue: 1, ExpireStyle: "day",
	})
	require.NoError(t, err)

	// 空投递
	_, err = svc.Submit(ctx, created.Token, nil, "1.1.1.1")
	require.Error(t, err)

	// 文件数超限
	_, err = svc.Submit(ctx, created.Token, []storage.StoredFileEntry{
		{RelPath: "uploads/a.bin", FileName: "a.bin", Size: 1},
		{RelPath: "uploads/b.bin", FileName: "b.bin", Size: 1},
	}, "1.1.1.1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "最多")

	// 总大小超限
	_, err = svc.Submit(ctx, created.Token, []storage.StoredFileEntry{
		{RelPath: "uploads/big.bin", FileName: "big.bin", Size: 9999},
	}, "1.1.1.1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "大小")

	// 未知 token
	_, err = svc.Submit(ctx, "deadbeef", []storage.StoredFileEntry{
		{RelPath: "uploads/a.bin", FileName: "a.bin", Size: 1},
	}, "1.1.1.1")
	require.Error(t, err)
}

// TestGetPublic_UnknownToken 公开查面：未知 token ⇒ 报错（不泄露存在性）。
func TestGetPublic_UnknownToken(t *testing.T) {
	svc, _ := newReqTestService(t)
	_, err := svc.GetPublic(context.Background(), "nosuchtoken")
	require.Error(t, err)
}

// TestCreateFederationSlot 跨站投递槽位（联邦前台化，2026-11-01）：
//
//	· token = 32 hex（**寄件码 token / 投递链接令牌**，不是 6 位取件码——此时
//	  还没有文件，对方柜不可能铸出取件码）；
//	· 属主 = 0（无本地用户 ⇒ 落成的是本站的匿名分享，归属天然正确）；
//	· 短 TTL（约 2 小时：铸出后立即用于转发，不留长命窗口）；
//	· 发起柜名与留言并进标题（投递链接模型只有 Title 一个可写字段 ⇒ 不静默丢弃）。
//
// 负对照：把 UserID 设成 1（或把标题里的 from_name/note 丢掉）⇒ 对应断言必红。
func TestCreateFederationSlot(t *testing.T) {
	svc, _ := newReqTestService(t)
	ctx := context.Background()

	fr, err := svc.CreateFederationSlot(ctx, "标题", "A 柜", "留言")
	require.NoError(t, err)
	assert.Len(t, fr.Token, 32, "投递令牌是 32 位 hex（不是 6 位取件码）")
	assert.Equal(t, uint(0), fr.UserID, "槽位无本地属主（落成匿名分享）")
	require.NotNil(t, fr.ExpiredAt)
	assert.WithinDuration(t, time.Now().Add(FederationSlotTTL), *fr.ExpiredAt, time.Minute,
		"槽位是短 TTL（过期后链接自然失效）")
	assert.Contains(t, fr.Title, "标题")
	assert.Contains(t, fr.Title, "A 柜")
	assert.Contains(t, fr.Title, "留言")

	// 空参数 ⇒ 仍有标题（不留空白标题）
	fr2, err := svc.CreateFederationSlot(ctx, "", "", "")
	require.NoError(t, err)
	assert.NotEmpty(t, fr2.Title)
	assert.NotEqual(t, fr.Token, fr2.Token, "每次铸槽位都是新 token")
}
