// 管理端文件列表 item 的**上传者字段形状**必须锁住（2026-10-29）。
//
// 前端（frontend-core 的 Files.vue / Moderation.vue）按 `username` 取上传者名字，
// 取不到才回退显示 `ID: {user_id}`。字段名一旦漂移，前端会静默退回裸 ID，
// 且后端测试全绿（没人断言这个键）⇒ 必须在此机械锁住。
//
// 负对照：把 fileGovernanceItem 里的 "username" 行去掉，本用例即失败。
package admin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

func TestFileGovernanceItem_UploaderFields(t *testing.T) {
	uid := uint(7)
	fc := &model.FileCode{
		Code:     "GOV001",
		UserID:   &uid,
		Username: "alice",
		Size:     42,
	}

	item := fileGovernanceItem(fc)

	// 上传者：username（名字）+ user_id（回退与筛选用）两者都在
	require.Contains(t, item, "username", "item 必须带 username —— 前端按它显示上传者名字")
	assert.Equal(t, "alice", item["username"])
	require.Contains(t, item, "user_id")
	assert.Equal(t, &uid, item["user_id"])

	// 匿名分享：username 为空串（不是缺键）⇒ 前端按 falsy 回退「匿名」
	anon := fileGovernanceItem(&model.FileCode{Code: "GOV002"})
	require.Contains(t, anon, "username")
	assert.Equal(t, "", anon["username"])
	assert.Nil(t, anon["user_id"])
}
