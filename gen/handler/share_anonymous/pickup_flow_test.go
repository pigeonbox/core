// 取件码通道端到端回归（2026-10-24）
//
// 缺陷（事故级）：「按取件次数」的分享走**取件码（匿名）通道**时，
// retrieve 自己把次数扣掉 ⇒ 计数归零 ⇒ model.IsExpired()（当时把
// ExpiredCount==0 也算过期）⇒ 随后的 /share/download 被判「已过期」⇒
// **末次取件必然 404、文件永远拿不到**；而分享页通道（select → download）
// 同样场景却能拿到 ⇒ 两条通道对同一条分享给出**相反**结论。
//
// 修复：扣次统一在**内容交付时**（按取件会话/下载令牌计次），retrieve 只发凭证；
// 「次数耗尽」也不再混作「过期」。
//
// 断言必须能变红：把 retrieve 还原成「自己扣次数」，本用例即失败。
package share_anonymous

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

	anonapp "github.com/pigeonbox/core/app/anonymous"
	shareService "github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/conf"
	sharehandler "github.com/pigeonbox/core/gen/handler/share"
	"github.com/pigeonbox/core/pkg/security"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
)

const anonRelPath = "uploads/anon-test.bin"
const anonBody = "anonymous-file-body"

// newAnonFlowEnv 构造「取件码 → retrieve → download」全链路测试环境。
func newAnonFlowEnv(t *testing.T, expiredCount int) (*gorm.DB, string) {
	t.Helper()

	dbDir, err := os.MkdirTemp("", "bq-anon-*")
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
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{},
		&model.PickupCharge{}, &model.TransferLog{}))

	dataPath := t.TempDir()
	full := filepath.Join(dataPath, anonRelPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(anonBody), 0o644))

	// 令牌开启（生产默认）⇒ 下载必须带令牌 ⇒ 令牌 = 一次取件的凭证
	conf.SetGlobalConfig(&conf.AppConfiguration{
		Security: conf.SecurityConfig{
			DownloadToken: conf.DownloadTokenConfig{Enabled: true, ValiditySeconds: 1000},
		},
	})
	security.SetDownloadTokenSecret("test-secret-32-chars-xxxxxxxxxxxxx")

	// 匿名取件域（内存 KV 模式：redis 未配置 ⇒ 单机内存）
	anonSvc = anonapp.NewService(nil, g)
	SetService(anonSvc)
	// 分享域（供 /share/download 用）
	sharehandler.SetShareService(shareService.NewService(g, "http://test.local",
		storage.NewStorageService(&storage.StorageConfig{
			Type:     storage.StorageTypeLocal,
			DataPath: dataPath,
		})))
	t.Cleanup(func() {
		anonSvc = nil
		conf.SetGlobalConfig(nil)
	})

	// 建分享：8 位分享码 ANON01 + 6 位取件码 PICK01，次数 = expiredCount
	repo := dao.NewFileCodeRepository(g)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "ANON01", UUIDFileName: "anon-test.bin", FilePath: anonRelPath,
		Size: int64(len(anonBody)), ExpiredCount: expiredCount,
	}))
	set, err := repo.SetPickupCode(ctx, "ANON01", "PICK01")
	require.NoError(t, err)
	require.True(t, set)
	return g, "PICK01"
}

// performAnonFlow 走一遍：POST /anonymous/retrieve → GET 下载直链。
//
// 取件查询失败时（如次数已用完）返回 (code, body, 0, "")，由调用方断言。
func performAnonFlow(t *testing.T, code string) (int, string, int, string) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/anonymous/retrieve", Retrieve)
	h.GET("/share/download", sharehandler.DownloadFile)

	// 取件码经 query 传（hertz 默认绑定器同时吃 query/form/json；query 最稳，
	// 不依赖请求体解析路径）。
	w := ut.PerformRequest(h.Engine, "POST", "/anonymous/retrieve?code="+url.QueryEscape(code),
		nil, ut.Header{Key: "Host", Value: "test.local"})
	retrieveCode, retrieveBody := w.Code, w.Body.String()
	if retrieveCode != 200 {
		return retrieveCode, retrieveBody, 0, ""
	}

	var resp struct {
		Code int `json:"code"`
		Data struct {
			DownloadURL string `json:"download_url"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(retrieveBody), &resp))
	// 业务码非 0（如 20007 次数已用完）⇒ 不下发直链，交给调用方断言
	if resp.Code != 0 {
		return retrieveCode, retrieveBody, 0, ""
	}
	require.NotEmpty(t, resp.Data.DownloadURL, "retrieve 应下发下载直链：%s", retrieveBody)

	w2 := ut.PerformRequest(h.Engine, "GET", resp.Data.DownloadURL, nil,
		ut.Header{Key: "Host", Value: "test.local"})
	return retrieveCode, retrieveBody, w2.Code, w2.Body.String()
}

// TestRetrieveThenDownload_LastPickupDelivers 末次取件必须真的拿到文件
//
// count=1 的分享：取件码通道取件 → 下载 → 必须 200 + 文件内容。
func TestRetrieveThenDownload_LastPickupDelivers(t *testing.T) {
	g, code := newAnonFlowEnv(t, 1)

	retrieveCode, retrieveBody, dlCode, dlBody := performAnonFlow(t, code)
	require.Equal(t, 200, retrieveCode, "取件查询应成功：%s", retrieveBody)
	assert.Equal(t, 200, dlCode, "末次取件的下载必须成功（此前必然 404「已过期」）：%s", dlBody)
	assert.Equal(t, anonBody, dlBody, "末次取件必须拿到文件内容")

	fc, err := dao.NewFileCodeRepository(g).GetByCode(context.Background(), "ANON01")
	require.NoError(t, err)
	assert.Equal(t, 0, fc.ExpiredCount, "交付时扣 1 次")
	assert.Equal(t, 1, fc.UsedCount)
}

// TestRetrieveThenDownload_ExhaustedIsNotExpired 次数耗尽 ⇒ 明确的「已用完」
//
// 交付完最后一次后再次取件 ⇒ 20007「取件码已用完」，**不是**「已过期」404。
// （此前两者都落在 IsExpired 里 ⇒ 返回「取件码已过期」20006。）
func TestRetrieveThenDownload_ExhaustedIsNotExpired(t *testing.T) {
	g, code := newAnonFlowEnv(t, 1)

	_, _, dlCode, dlBody := performAnonFlow(t, code)
	require.Equal(t, 200, dlCode, "末次取件应交付成功：%s", dlBody)

	// 第二次取件：次数已用完 ⇒ 独立业务码 20007（不是「已过期」20006）
	_, retrieveBody, _, _ := performAnonFlow(t, code)
	var biz struct {
		Code int `json:"code"`
	}
	require.NoError(t, json.Unmarshal([]byte(retrieveBody), &biz))
	assert.Equal(t, 20007, biz.Code, "应是 20007「取件码已用完」：%s", retrieveBody)

	fc, err := dao.NewFileCodeRepository(g).GetByCode(context.Background(), "ANON01")
	require.NoError(t, err)
	assert.Equal(t, 1, fc.UsedCount, "耗尽后不得再累加 used_count")
}
