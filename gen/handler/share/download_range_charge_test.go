// core#171 回归：Range 416 在**扣次之后**返回 ⇒ 违反取件计次设计稿不变量 4
// 「失败不消耗次数」（真相源 docs/specs/2026-10-24-pickup-count-semantics-design.md §3）。
//
// 一次零字节交付烧掉一次取件额度：owner 的 N 次可在「什么都没拿到」时被耗光，
// 配合每次 /share/select 换新令牌即可逐次消耗。
//
// 断言必须能变红：把 Range 可满足性判定挪回 chargePickup 之后 ⇒ 416 用例当场红。
package share

import (
	"net/url"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
)

// performDownloadWithRange 发起带 Range 头的下载请求。
func performDownloadWithRange(t *testing.T, query, rangeHdr string) (int, string) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/share/download", DownloadFile)
	w := ut.PerformRequest(h.Engine, "GET", "/share/download?"+query, nil,
		ut.Header{Key: "Host", Value: "test.local"},
		ut.Header{Key: "Range", Value: rangeHdr})
	return w.Code, w.Body.String()
}

// TestDownload_UnsatisfiableRange_DoesNotConsumeCount 单文件分支。
func TestDownload_UnsatisfiableRange_DoesNotConsumeCount(t *testing.T) {
	g := newChargeTestEnv(t, true)
	repo := dao.NewFileCodeRepository(g)
	require.NoError(t, repo.Create(t.Context(), &model.FileCode{
		Code: "RANGE01", Text: "a.bin", FilePath: "uploads", UUIDFileName: "a.bin",
		Size: 10, ExpiredCount: 1, // 次数=1：416 一旦扣次，后续就再也取不到
	}))
	resp := selectShare(t, "RANGE01")
	require.NotEmpty(t, resp.Data.Token)
	tok := "&token=" + url.QueryEscape(resp.Data.Token)

	// ① 越界 Range（start ≥ 文件大小 10）⇒ 416，且**不得扣次**
	code, body := performDownloadWithRange(t, "code=RANGE01"+tok, "bytes=999999-")
	assert.Equal(t, 416, code, "越界 Range 应 416：%s", body)
	remaining, used := usedCount(t, g, "RANGE01")
	assert.Equal(t, 1, remaining, "416 是失败交付 ⇒ 不得消耗取件次数（core#171）")
	assert.Equal(t, 0, used, "416 不得记取件（修复前此处已扣 ⇒ 复现靶心）")

	// ② 合法 Range ⇒ 正常扣次（防止修复成「永远不扣」的假绿）
	code, body = performDownloadWithRange(t, "code=RANGE01"+tok, "bytes=0-4")
	assert.Equal(t, 206, code, "合法 Range 应 206 交付：%s", body)
	remaining, used = usedCount(t, g, "RANGE01")
	assert.Equal(t, 0, remaining, "合法 Range 是成功交付 ⇒ 应扣 1 次")
	assert.Equal(t, 1, used)
}

// TestDownload_ChildUnsatisfiableRange_DoesNotConsumeCount 子文件分支（同序缺陷，
// issue 点名 :881）：?file=<id> + 越界 Range 同样不得扣次。
func TestDownload_ChildUnsatisfiableRange_DoesNotConsumeCount(t *testing.T) {
	g := newChargeTestEnv(t, true)
	repo := dao.NewFileCodeRepository(g)
	require.NoError(t, repo.Create(t.Context(), &model.FileCode{
		Code: "RANGE02", Text: "a.bin", FilePath: "uploads", UUIDFileName: "a.bin",
		Size: 10, ExpiredCount: 1,
	}))
	fileRepo := dao.NewFileCodeFileRepository(g)
	require.NoError(t, fileRepo.CreateBatch(t.Context(), []*model.FileCodeFile{
		{FileCodeID: 1, FilePath: "uploads/a.bin", UUIDFileName: "a.bin", Size: 10},
	}))
	children, err := fileRepo.ListByFileCodeID(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, children, 1)

	resp := selectShare(t, "RANGE02")
	require.NotEmpty(t, resp.Data.Token)
	tok := "&token=" + url.QueryEscape(resp.Data.Token)

	code, body := performDownloadWithRange(t,
		"code=RANGE02&file="+itoa(children[0].ID)+tok, "bytes=999999-")
	assert.Equal(t, 416, code, "子文件越界 Range 应 416：%s", body)
	remaining, used := usedCount(t, g, "RANGE02")
	assert.Equal(t, 1, remaining, "子文件 416 不得消耗取件次数（core#171）")
	assert.Equal(t, 0, used)
}
