// /share/download 的 `file` 子文件参数**严格解析**守卫（core#148，#102/#104 同族）。
//
// 缺陷形态：`strconv.ParseUint` 失败被静默吞掉（fileID 保持 0）⇒ 请求被当成
// 「未指定子文件」处理：
//
//	GET /share/download?code=X&file=abc（打错/编码错）
//	  ⇒ 落到「多文件打包 zip」分支 ⇒ 200 返回**整包**，且**吃掉一次取件次数**；
//	  调用方以为下载的是某一个子文件。
//
// 判据：file 非法（非正整数）⇒ 400 + 业务码 10001 + 点名参数，且**不扣次**
// （失败发生在扣次之前）。
// 负对照：把解析错误分支改回「忽略」⇒ 本文件转红。
package share

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// newFileParamShare 造一条 2 子文件的分享（复用计次夹具：文件已在盘上）。
func newFileParamShare(t *testing.T, code string) *gorm.DB {
	t.Helper()
	g := newChargeTestEnv(t, false)
	ctx := context.Background()
	repo := dao.NewFileCodeRepository(g)
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: code, Text: "a.bin", FilePath: "uploads", UUIDFileName: "a.bin",
		Size: 10, ExpiredCount: 1,
	}))
	fileRepo := dao.NewFileCodeFileRepository(g)
	require.NoError(t, fileRepo.CreateBatch(ctx, []*model.FileCodeFile{
		{FileCodeID: 1, FilePath: "uploads/a.bin", UUIDFileName: "a.bin", Size: 10},
		{FileCodeID: 1, FilePath: "uploads/b.bin", UUIDFileName: "b.bin", Size: 10},
	}))
	return g
}

// TestDownloadFile_NonNumericFileParam_Rejected file=非数字 ⇒ 400 + 10001 + 点名参数，
// 且不消耗取件次数（不得退化成整包下载）。
func TestDownloadFile_NonNumericFileParam_Rejected(t *testing.T) {
	newFileParamShare(t, "FPARM01")

	code, body := performDownloadPath(t, "code=FPARM01&file=abc")
	assert.Equal(t, 400, code, "非法 file 参数应 400（不得静默当未指定而返回整包）：%s", body)
	assert.Contains(t, body, `"code":10001`, "业务码应为 10001（参数错误）：%s", body)
	assert.Contains(t, body, "file", "错误消息必须点名参数：%s", body)
}

// TestDownloadFile_ZeroFileParam_Rejected file=0 不是合法子文件 id ⇒ 同口径 400。
func TestDownloadFile_ZeroFileParam_Rejected(t *testing.T) {
	newFileParamShare(t, "FPARM02")

	code, body := performDownloadPath(t, "code=FPARM02&file=0")
	assert.Equal(t, 400, code, "file=0 应 400：%s", body)
	assert.Contains(t, body, `"code":10001`, "%s", body)
}

// TestDownloadFile_BadFileParam_DoesNotConsumePickup 失败请求不得吃次数
// （与既有「归属校验失败不扣次」同口径）。
func TestDownloadFile_BadFileParam_DoesNotConsumePickup(t *testing.T) {
	g := newFileParamShare(t, "FPARM03")

	code, _ := performDownloadPath(t, "code=FPARM03&file=notanumber")
	require.Equal(t, 400, code)

	repo := dao.NewFileCodeRepository(g)
	fc, err := repo.GetByCode(context.Background(), "FPARM03")
	require.NoError(t, err)
	assert.Equal(t, 1, fc.ExpiredCount, "参数错误的请求不得消耗取件次数")
	assert.Equal(t, 0, fc.UsedCount, "参数错误的请求不得记取件")
}

// TestDownloadFile_ValidFileParam_StillWorks 收紧不得误伤：合法子文件 id 照旧 200。
func TestDownloadFile_ValidFileParam_StillWorks(t *testing.T) {
	g := newFileParamShare(t, "FPARM04")

	children, err := dao.NewFileCodeFileRepository(g).ListByFileCodeID(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, children, 2)

	code, body := performDownloadPath(t, "code=FPARM04&file="+itoa(children[0].ID))
	assert.Equal(t, 200, code, "合法 file 参数必须仍可下载：%s", body)
}
