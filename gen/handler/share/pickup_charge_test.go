// 取件计次回归测试（2026-10-24）
//
// 覆盖的缺陷（均为真实可复现的语义缺陷，不是风格问题）：
//
//  1. 多文件分享逐文件下载**逐个扣次** ⇒ 「取件次数=1 的多文件分享只能下载
//     第一个文件」，而「打包下载」同一个 zip 只扣 1 次 ⇒ 两条 UI 路径的次数
//     语义不可比。
//  2. 扣次发生在**鉴权段末尾**，早于子文件归属校验 ⇒ `file=` 不属于该分享时
//     返回 404，但取件次数已被吃掉（白扔一次取件机会）。
//  3. 「按取件次数」的分享，**末次取件拿不到文件**：匿名 retrieve 自己扣次数
//     ⇒ 计数归零 ⇒ IsExpired ⇒ 随后的下载被判「已过期」⇒ 404。
//  4. 文本分享在 select 即交付正文却从不扣次 ⇒ 「按取件次数」对文本分享
//     形同虚设。
//  5. 下载端点按**请求原始码**校验令牌，而签发按规范码 ⇒ 用户把下载 URL 的
//     code 换成 6 位取件码 ⇒ 401「令牌无效」（令牌本身是好的）。
//
// 断言原则：每条断言都要能**变红**（抽掉修复即失败），不做恒真断言。
package share

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	shareService "github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/security"
	"github.com/pigeonbox/core/pkg/transfer"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
)

// newChargeTestEnv 构造带计次表的 handler 测试环境（令牌**开启**）。
func newChargeTestEnv(t *testing.T, tokenEnabled bool) *gorm.DB {
	t.Helper()

	dbDir, err := os.MkdirTemp("", "bq-charge-*")
	require.NoError(t, err)
	g, err := gorm.Open(sqlite.Open(filepath.Join(dbDir, "test.db")), &gorm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() {
		if sqlDB, dbErr := g.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
		for i := 0; i < 10; i++ {
			if rmErr := os.RemoveAll(dbDir); rmErr == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	// 计次表必须 AutoMigrate（生产走版本化迁移 + AutoMigrate 兜底）
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{},
		&model.PickupCharge{}, &model.TransferLog{}))
	transfer.SetSink(daoTransferLogSink{gormDB: g})
	t.Cleanup(func() { transfer.Flush(3 * time.Second); transfer.SetSink(nil) })

	dataPath := t.TempDir()
	for _, name := range []string{"a.bin", "b.bin"} {
		p := filepath.Join(dataPath, "uploads", name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("body-"+name), 0o644))
	}

	conf.SetGlobalConfig(&conf.AppConfiguration{
		Security: conf.SecurityConfig{
			DownloadToken: conf.DownloadTokenConfig{Enabled: tokenEnabled, ValiditySeconds: 1000},
		},
	})
	security.SetDownloadTokenSecret("test-secret-32-chars-xxxxxxxxxxxxx")

	// 走 setter（带锁）⇒ 与并发读方同一把锁 ⇒ 无 DATA RACE
	SetShareService(shareService.NewService(g, "http://test.local", storage.NewStorageService(&storage.StorageConfig{
		Type:     storage.StorageTypeLocal,
		DataPath: dataPath,
	})))
	t.Cleanup(func() {
		// 复位也走 setter（带锁）⇒ 与异步任务的读并发时仍是安全的
		SetShareService(nil)
		conf.SetGlobalConfig(nil)
	})
	return g
}

// performGet 发起一次 GET（handler 直挂，避开 router import cycle）。
func performGet(t *testing.T, path, query string, headers ut.Header) (int, string) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET(path, GetShare)
	w := ut.PerformRequest(h.Engine, "GET", path+"?"+query, nil, headers)
	return w.Code, w.Body.String()
}

// performDownloadPath 发起一次下载请求。
func performDownloadPath(t *testing.T, query string) (int, string) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/share/download", DownloadFile)
	w := ut.PerformRequest(h.Engine, "GET", "/share/download?"+query, nil,
		ut.Header{Key: "Host", Value: "test.local"})
	return w.Code, w.Body.String()
}

// shareSelectResp select 响应（取 token / download_url）。
type shareSelectResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Code        string `json:"code"`
		Text        string `json:"text"`
		DownloadURL string `json:"download_url"`
		Token       string `json:"token"`
		HasPassword bool   `json:"has_password"`
	} `json:"data"`
}

func selectShare(t *testing.T, code string) shareSelectResp {
	t.Helper()
	status, body := performGet(t, "/share/select/", "code="+url.QueryEscape(code),
		ut.Header{Key: "Host", Value: "test.local"})
	require.Equal(t, 200, status, "select 应成功：%s", body)
	var resp shareSelectResp
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	return resp
}

func usedCount(t *testing.T, g *gorm.DB, code string) (int, int) {
	t.Helper()
	fc, err := dao.NewFileCodeRepository(g).GetByCode(context.Background(), code)
	require.NoError(t, err)
	return fc.ExpiredCount, fc.UsedCount
}

// =====================================================================
// 1. 多文件逐文件下载：同一次取件只扣 1 次
// =====================================================================
func TestMultiFileDownload_ChargesOncePerPickup(t *testing.T) {
	// 令牌开启（生产默认）：**令牌 = 一次取件的凭证** ⇒ 同一次取件（同一令牌）
	// 下的多个文件只扣 1 次。
	g := newChargeTestEnv(t, true)
	ctx := context.Background()
	repo := dao.NewFileCodeRepository(g)
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "MULTI01", Text: "a.bin", FilePath: "uploads", UUIDFileName: "a.bin",
		Size: 10, ExpiredCount: 1, // 次数=1：按「一次取件」语义应能取到全部文件
	}))
	fileRepo := dao.NewFileCodeFileRepository(g)
	require.NoError(t, fileRepo.CreateBatch(ctx, []*model.FileCodeFile{
		{FileCodeID: 1, FilePath: "uploads/a.bin", UUIDFileName: "a.bin", Size: 10},
		{FileCodeID: 1, FilePath: "uploads/b.bin", UUIDFileName: "b.bin", Size: 10},
	}))
	children, err := fileRepo.ListByFileCodeID(ctx, 1)
	require.NoError(t, err)
	require.Len(t, children, 2)
	childA, childB := children[0], children[1]

	// 取件凭证（select 下发的下载令牌）——同一次取件用同一个令牌
	resp := selectShare(t, "MULTI01")
	require.NotEmpty(t, resp.Data.Token)
	tok := "&token=" + url.QueryEscape(resp.Data.Token)

	// 第一次：逐个下载两个文件
	code, body := performDownloadPath(t, "code=MULTI01&file="+itoa(childA.ID)+tok)
	assert.Equal(t, 200, code, "第一个子文件应可下载：%s", body)

	code, body = performDownloadPath(t, "code=MULTI01&file="+itoa(childB.ID)+tok)
	assert.Equal(t, 200, code, "同一次取件的第二个子文件必须仍可下载（次数=1）：%s", body)

	remaining, used := usedCount(t, g, "MULTI01")
	assert.Equal(t, 0, remaining, "一次取件扣 1 次")
	assert.Equal(t, 1, used, "两个子文件下载只记 1 次取件（此前记 2 次）")
}

// TestMultiFileDownload_NoToken_StillChargesOnce 令牌关闭时的兜底会话键
//
// security.download_token.enabled=false ⇒ 没有令牌 ⇒ 没有天然的「取件会话」
// 凭证。兜底键 = (分享码, 取件人 IP) ⇒ 同一收件人的多文件下载仍只扣 1 次
// （否则关闭令牌时「次数=1 只能下载第一个文件」的老问题重现）。
func TestMultiFileDownload_NoToken_StillChargesOnce(t *testing.T) {
	g := newChargeTestEnv(t, false) // 令牌关闭
	ctx := context.Background()
	repo := dao.NewFileCodeRepository(g)
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "MULTI02", Text: "a.bin", FilePath: "uploads", UUIDFileName: "a.bin",
		Size: 10, ExpiredCount: 1,
	}))
	fileRepo := dao.NewFileCodeFileRepository(g)
	require.NoError(t, fileRepo.CreateBatch(ctx, []*model.FileCodeFile{
		{FileCodeID: 1, FilePath: "uploads/a.bin", UUIDFileName: "a.bin", Size: 10},
		{FileCodeID: 1, FilePath: "uploads/b.bin", UUIDFileName: "b.bin", Size: 10},
	}))
	children, err := fileRepo.ListByFileCodeID(ctx, 1)
	require.NoError(t, err)
	require.Len(t, children, 2)

	code, body := performDownloadPath(t, "code=MULTI02&file="+itoa(children[0].ID))
	assert.Equal(t, 200, code, "第一个子文件应可下载：%s", body)
	code, body = performDownloadPath(t, "code=MULTI02&file="+itoa(children[1].ID))
	assert.Equal(t, 200, code, "无令牌时同收件人的第二个子文件仍应可下载：%s", body)

	remaining, used := usedCount(t, g, "MULTI02")
	assert.Equal(t, 0, remaining, "一次取件扣 1 次")
	assert.Equal(t, 1, used, "无令牌兜底键下也只记 1 次取件")
}

// =====================================================================
// 2. 失败请求不吃次数（归属校验失败 ⇒ 不扣次）
// =====================================================================
func TestDownload_ForeignChildFile_DoesNotConsumeCount(t *testing.T) {
	g := newChargeTestEnv(t, false)
	ctx := context.Background()
	repo := dao.NewFileCodeRepository(g)
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "FOREIGN", Text: "a.bin", FilePath: "uploads", UUIDFileName: "a.bin",
		Size: 10, ExpiredCount: 1,
	}))
	// 另一个分享的子文件
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "OTHER01", ExpiredCount: -1}))
	otherRepo := dao.NewFileCodeFileRepository(g)
	require.NoError(t, otherRepo.CreateBatch(ctx, []*model.FileCodeFile{
		{FileCodeID: 2, FilePath: "uploads/b.bin", UUIDFileName: "b.bin", Size: 10},
	}))
	others, err := otherRepo.ListByFileCodeID(ctx, 2)
	require.NoError(t, err)
	require.Len(t, others, 1)
	other := others[0]

	code, _ := performDownloadPath(t, "code=FOREIGN&file="+itoa(other.ID))
	assert.Equal(t, 404, code, "不属于该分享的子文件应 404")

	remaining, used := usedCount(t, g, "FOREIGN")
	assert.Equal(t, 1, remaining, "失败请求**不得**消耗取件次数（此前扣次在归属校验之前）")
	assert.Equal(t, 0, used)
}

// =====================================================================
// 3. 末次取件必须拿得到文件（count=1 的分享）
// =====================================================================
func TestDownload_LastPickupSucceeds(t *testing.T) {
	g := newChargeTestEnv(t, false)
	ctx := context.Background()
	require.NoError(t, dao.NewFileCodeRepository(g).Create(ctx, &model.FileCode{
		Code: "LASTONE", Text: "a.bin", FilePath: "uploads", UUIDFileName: "a.bin",
		Size: 10, ExpiredCount: 1,
	}))

	code, body := performDownloadPath(t, "code=LASTONE")
	assert.Equal(t, 200, code, "末次取件必须拿到文件：%s", body)

	remaining, used := usedCount(t, g, "LASTONE")
	assert.Equal(t, 0, remaining)
	assert.Equal(t, 1, used)
}

// =====================================================================
// 4. 文本分享：select 交付正文 ⇒ 扣次
// =====================================================================
func TestSelect_TextShare_ChargesCount(t *testing.T) {
	g := newChargeTestEnv(t, false)
	ctx := context.Background()
	require.NoError(t, dao.NewFileCodeRepository(g).Create(ctx, &model.FileCode{
		Code: "TEXT001", Text: "hello-text", FilePath: "", ExpiredCount: 2,
	}))

	resp := selectShare(t, "TEXT001")
	assert.Equal(t, "hello-text", resp.Data.Text, "文本分享应在 select 直接交付正文")

	remaining, used := usedCount(t, g, "TEXT001")
	assert.Equal(t, 1, remaining, "文本分享交付正文应扣 1 次（此前从不扣 ⇒ 次数护栏失效）")
	assert.Equal(t, 1, used)
}

// 文本分享次数用尽 ⇒ 明确 10003，不再是 404「不存在/已过期」
func TestSelect_CountExhausted_ReturnsForbidden(t *testing.T) {
	g := newChargeTestEnv(t, false)
	ctx := context.Background()
	require.NoError(t, dao.NewFileCodeRepository(g).Create(ctx, &model.FileCode{
		Code: "TEXT002", Text: "hello-text", FilePath: "", ExpiredCount: 0,
	}))

	status, body := performGet(t, "/share/select/", "code=TEXT002",
		ut.Header{Key: "Host", Value: "test.local"})
	assert.Equal(t, 403, status, "次数用尽应是 403（20004），不是 404「不存在/已过期」：%s", body)
	assert.Contains(t, body, "20004", "业务码应是 20004「超过取件次数」：%s", body)
}

// =====================================================================
// 5. 令牌按规范码校验：6 位取件码 + 令牌 ⇒ 可下载
// =====================================================================
func TestDownload_TokenVerifiedAgainstCanonicalCode(t *testing.T) {
	g := newChargeTestEnv(t, true)
	ctx := context.Background()
	repo := dao.NewFileCodeRepository(g)
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "CANON01", Text: "a.bin", FilePath: "uploads", UUIDFileName: "a.bin",
		Size: 10, ExpiredCount: -1,
	}))
	// 铸造 6 位取件码（真相源列 pickup_code）
	set, err := repo.SetPickupCode(ctx, "CANON01", "PICK01")
	require.NoError(t, err)
	require.True(t, set, "取件码应铸造成功")

	// select 用 6 位取件码取件 ⇒ 服务端下发按**规范码**签发的令牌
	resp := selectShare(t, "PICK01")
	require.NotEmpty(t, resp.Data.Token, "select 应下发下载令牌")

	// 下载 URL 用 6 位取件码 + 该令牌 ⇒ 必须能下载（此前按原始码验签 ⇒ 401）
	code, body := performDownloadPath(t, "code=PICK01&token="+url.QueryEscape(resp.Data.Token))
	assert.Equal(t, 200, code, "6 位取件码 + 令牌应可下载（令牌按规范码校验）：%s", body)

	// 令牌不可跨**分享**使用（PICK01 与 CANON01 是同一条分享的两个码形态，
	// 令牌对两者都有效才是正确的；真正的越权是拿到别的分享上去用）。
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "CANON02", Text: "a.bin", FilePath: "uploads", UUIDFileName: "a.bin",
		Size: 10, ExpiredCount: -1,
	}))
	code, _ = performDownloadPath(t, "code=CANON02&token="+url.QueryEscape(resp.Data.Token))
	assert.Equal(t, 401, code, "令牌绑定分享码，不可跨分享使用")
}

// itoa 免 strconv 导出的极简转换（测试内用）。
func itoa(v uint) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
