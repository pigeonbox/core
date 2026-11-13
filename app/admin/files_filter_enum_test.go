// ParseFilesFilter 的**枚举型**过滤参数守卫（core#147，core#102 的漏网同族）。
//
// 缺陷形态：health / deleted 是 switch 枚举，**没有 default 分支** ⇒
// 非法取值 ⇒ 条件根本不落 ⇒ 查询退化成**全量**（正是 #102 点名的
// 「筛选失败比筛选为空危险：多给数据」）：
//
//	GET /admin/files/filter?health=expiringSoon（大小写/拼写错）⇒ 返回全部分享，
//	而调用方（仪表盘健康卡直达链接 ?health=expiring_soon）以为看的是 24h 内到期。
//
// 判据：非法枚举 ⇒ error（handler 统一 400 + 10001 + 点名参数）；
// 合法值与空串（= 不过滤）照旧放行。
// 负对照：把两处 default 拒绝分支删掉 ⇒ 本文件转红。
package admin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// filterQuery 把一组 query 取值塞进 ParseFilesFilter 的取值函数。
func filterQuery(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestParseFilesFilter_HealthEnumRejected(t *testing.T) {
	valid := []string{"", "active", "expired", "expiring_soon", "never_picked", "forever"}
	for _, v := range valid {
		q, err := ParseFilesFilter(filterQuery(map[string]string{"health": v}))
		require.NoError(t, err, "合法 health=%q 应放行", v)
		assert.Equal(t, v, q.Health, "合法值原样透传")
	}

	for _, bad := range []string{"expiringSoon", "Expiredd", "soon", "1"} {
		_, err := ParseFilesFilter(filterQuery(map[string]string{"health": bad}))
		require.Error(t, err, "非法 health=%q 必须报错（不得静默丢条件返回全量）", bad)
		assert.Contains(t, err.Error(), "health", "错误必须点名参数：%v", err)
	}
}

func TestParseFilesFilter_DeletedEnumRejected(t *testing.T) {
	valid := []string{"", "all", "only"}
	for _, v := range valid {
		q, err := ParseFilesFilter(filterQuery(map[string]string{"deleted": v}))
		require.NoError(t, err, "合法 deleted=%q 应放行", v)
		assert.Equal(t, v, q.Deleted, "合法值原样透传")
	}

	for _, bad := range []string{"onlyy", "true", "ALLL"} {
		_, err := ParseFilesFilter(filterQuery(map[string]string{"deleted": bad}))
		require.Error(t, err, "非法 deleted=%q 必须报错", bad)
		assert.Contains(t, err.Error(), "deleted", "错误必须点名参数：%v", err)
	}
}

// TestParseFilesFilter_OtherParamsUnchanged 收紧枚举不得误伤既有口径：
// 其余参数（keyword/status/upload_type 自由串、数值/时间/布尔/分页）一字不改。
func TestParseFilesFilter_OtherParamsUnchanged(t *testing.T) {
	q, err := ParseFilesFilter(filterQuery(map[string]string{
		"keyword":   "abc",
		"status":    "normal",
		"min_size":  "10",
		"max_size":  "20",
		"expired":   "true",
		"page":      "2",
		"page_size": "50",
	}))
	require.NoError(t, err)
	assert.Equal(t, "abc", q.Keyword)
	assert.Equal(t, "normal", q.Status)
	require.NotNil(t, q.MinSize)
	assert.Equal(t, int64(10), *q.MinSize)
	require.NotNil(t, q.MaxSize)
	assert.Equal(t, int64(20), *q.MaxSize)
	require.NotNil(t, q.Expired)
	assert.True(t, *q.Expired)
	assert.Equal(t, 2, q.Page)
	assert.Equal(t, 50, q.PageSize)
}
