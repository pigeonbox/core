package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// ── 管理员口令**原地**重置（fnos#182）──────────────────────────────────
//
// 背景：fnOS/QNAP 向导的「重置管理员口令」此前是平台层**删 admin 行**
// （DELETE FROM users WHERE username='admin'）＋ core 启动时按 PB_ADMIN_PASSWORD
// 重建 ⇒ admin 的 user id 变化 ⇒ 名下 file_codes / user_api_keys 全部成孤儿
// （真机 88 份分享 + 3 把 Key）。
//
// ⇒ 口令哈希与「改密」语义归 core（bcrypt 唯一真相源）：平台层只负责把新口令
// 交过来（一次性文件），core **原地**改哈希 ⇒ user id 不变 ⇒ 名下数据归属不变。
//
// 验收（issue 正文）：
//   ① 改口令后 admin id 不变；
//   ② file_codes / user_api_keys 行数与归属不变；
//   ③ 旧 API Key 仍能 200（API Key 鉴权不校验 session epoch，见 pkg/middleware/apikey.go）；
//   ④ 旧 JWT 失效（会话纪元 +1，与 app/user.ChangePassword 同口径）。

func newAdminResetTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.User{}, &model.FileCode{}, &model.UserAPIKey{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

// seedAdminWithAssets 建一个 admin（口令 old-pw）+ 名下 1 份分享 + 1 把 API Key。
func seedAdminWithAssets(t *testing.T, g *gorm.DB) *model.User {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("old-pw"), bcrypt.MinCost)
	require.NoError(t, err)
	admin := &model.User{
		Username: "admin", Email: "admin@pigeonbox.local",
		PasswordHash: string(hash), Role: "admin", Status: "active",
	}
	require.NoError(t, g.Create(admin).Error)
	require.NoError(t, g.Create(&model.FileCode{Code: "OWN0001", UserID: &admin.ID}).Error)
	require.NoError(t, g.Create(&model.UserAPIKey{
		UserID: admin.ID, Name: "prewire", KeyHash: "hash-of-old-key",
	}).Error)
	return admin
}

func writeResetFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".admin_password_reset")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	t.Setenv("PB_ADMIN_PASSWORD_RESET_FILE", path)
	return path
}

func TestApplyPendingAdminPasswordReset_PreservesUserID(t *testing.T) {
	g := newAdminResetTestDB(t)
	admin := seedAdminWithAssets(t, g)
	oldID := admin.ID

	path := writeResetFile(t, "new-pw")
	require.NoError(t, ApplyPendingAdminPasswordReset(g))

	// ① admin id 不变（**没有删号重建**）
	var after model.User
	require.NoError(t, g.Where("username = ?", "admin").First(&after).Error)
	assert.Equal(t, oldID, after.ID, "重置口令不得改变 admin 的 user id（否则名下数据成孤儿）")

	// 新口令生效、旧口令失效
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(after.PasswordHash), []byte("new-pw")),
		"新口令必须生效")
	assert.Error(t, bcrypt.CompareHashAndPassword([]byte(after.PasswordHash), []byte("old-pw")),
		"旧口令必须失效")

	// 旧 JWT 失效：会话纪元 +1（与 app/user.ChangePassword 同口径）
	assert.Equal(t, 1, after.SessionEpoch, "改密后必须 +1 会话纪元（旧 JWT 即时失效）")

	// ② 名下数据行数与归属不变
	var fcCount int64
	require.NoError(t, g.Model(&model.FileCode{}).Count(&fcCount).Error)
	assert.Equal(t, int64(1), fcCount, "名下分享行数不得变")
	var fc model.FileCode
	require.NoError(t, g.Where("code = ?", "OWN0001").First(&fc).Error)
	require.NotNil(t, fc.UserID)
	assert.Equal(t, oldID, *fc.UserID, "名下分享归属不得变（不得成孤儿）")

	var keyCount int64
	require.NoError(t, g.Model(&model.UserAPIKey{}).Count(&keyCount).Error)
	assert.Equal(t, int64(1), keyCount, "名下 API Key 行数不得变")
	var key model.UserAPIKey
	require.NoError(t, g.Where("key_hash = ?", "hash-of-old-key").First(&key).Error)
	assert.Equal(t, oldID, key.UserID, "名下 API Key 归属不得变（不得成孤儿）")
	assert.False(t, key.Revoked, "旧 API Key 不得被吊销（改密不影响 Key）")

	// 一次性文件已消费（否则每次启动都改一次 ⇒ 把用户后来改的口令又冲掉）
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "重置文件必须被消费（一次性语义）")
}

// 负对照：没有待处理文件 ⇒ 什么都不做（不得碰 admin）。
func TestApplyPendingAdminPasswordReset_NoFile_Noop(t *testing.T) {
	g := newAdminResetTestDB(t)
	admin := seedAdminWithAssets(t, g)
	t.Setenv("PB_ADMIN_PASSWORD_RESET_FILE", "")

	require.NoError(t, ApplyPendingAdminPasswordReset(g))

	var after model.User
	require.NoError(t, g.Where("username = ?", "admin").First(&after).Error)
	assert.Equal(t, admin.PasswordHash, after.PasswordHash, "无请求时不得改哈希")
	assert.Equal(t, 0, after.SessionEpoch)
}

// 负对照：文件里口令为空 ⇒ 报错并**保留文件**（不得把管理员锁在门外）。
func TestApplyPendingAdminPasswordReset_EmptyPassword_KeepsFile(t *testing.T) {
	g := newAdminResetTestDB(t)
	admin := seedAdminWithAssets(t, g)
	path := writeResetFile(t, "\n")

	err := ApplyPendingAdminPasswordReset(g)
	require.Error(t, err, "空口令必须报错")

	var after model.User
	require.NoError(t, g.Where("username = ?", "admin").First(&after).Error)
	assert.Equal(t, admin.PasswordHash, after.PasswordHash, "空口令不得改哈希")

	_, statErr := os.Stat(path)
	assert.False(t, os.IsNotExist(statErr), "失败时必须保留重置文件（下次启动重试）")
}

// 负对照：库里没有管理员 ⇒ 不凭空建号（PB_DISABLE_DEFAULT_ADMIN 语义不许被绕过），
// 文件保留，等管理员被创建后自然生效。
func TestApplyPendingAdminPasswordReset_NoAdmin_KeepsFile(t *testing.T) {
	g := newAdminResetTestDB(t)
	path := writeResetFile(t, "new-pw")

	require.NoError(t, ApplyPendingAdminPasswordReset(g))

	var admins int64
	require.NoError(t, g.Model(&model.User{}).Where("role = ?", "admin").Count(&admins).Error)
	assert.Equal(t, int64(0), admins, "不得凭空创建管理员")

	_, statErr := os.Stat(path)
	assert.False(t, os.IsNotExist(statErr), "无管理员时必须保留重置文件")
}
