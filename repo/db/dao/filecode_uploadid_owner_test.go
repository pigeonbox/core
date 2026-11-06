package dao

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

// 回归锁（2026-11-06 P0）：上传完成面的幂等回读**必须按属主限定**。
//
// 缺陷链条（三处配合才成立，缺一不可）：
//  1. 分片通道的 upload_id 是**客户端自报的 file_hash**（gen/handler/chunk
//     `uploadID := req.FileHash`）⇒ 任何持同一份文件的人都能算出他人的 upload_id；
//  2. init 对「已完成会话」的复活路径会把 owner_ip/user_id 改写成新调用方
//     （2026-10-30 为修「7 天内无法重传同一文件」的刻意设计）⇒ 归属校验通过；
//  3. 幂等回读此前**只看 upload_id**（FirstByUploadID，Order id ASC）⇒
//     命中**受害者最早那条分享** ⇒ 把受害者的分享码（连同其取件额度）交出去。
//
// 本文件锁住第 3 环：属主不符 ⇒ 未命中；同一 upload_id 有多条时取
// 「我自己的」而不是「id 最小（最早）的」。
func TestFirstByUploadIDOwnedBy(t *testing.T) {
	ctx := context.Background()
	uid := func(v uint) *uint { return &v }

	newRepo := func(t *testing.T) *FileCodeRepository {
		t.Helper()
		return NewFileCodeRepository(newGovernanceTestDB(t))
	}

	t.Run("登录属主命中", func(t *testing.T) {
		repo := newRepo(t)
		require.NoError(t, repo.Create(ctx, &model.FileCode{
			Code: "OWN00001", UploadID: "u-own-login", UserID: uid(1), OwnerIP: "10.0.0.1",
		}))
		fc, err := repo.FirstByUploadIDOwnedBy(ctx, "u-own-login", uid(1), "10.0.0.1")
		require.NoError(t, err)
		assert.Equal(t, "OWN00001", fc.Code)
	})

	t.Run("负对照：登录他人不命中（修复前会命中）", func(t *testing.T) {
		repo := newRepo(t)
		require.NoError(t, repo.Create(ctx, &model.FileCode{
			Code: "VICTM001", UploadID: "u-victim-login", UserID: uid(1), OwnerIP: "10.0.0.1",
		}))
		_, err := repo.FirstByUploadIDOwnedBy(ctx, "u-victim-login", uid(2), "10.0.0.2")
		require.True(t, errors.Is(err, gorm.ErrRecordNotFound),
			"他人分享不得按 upload_id 命中（否则等于泄露他人分享码）")
	})

	t.Run("负对照：匿名跨 IP 不命中", func(t *testing.T) {
		repo := newRepo(t)
		require.NoError(t, repo.Create(ctx, &model.FileCode{
			Code: "ANON0001", UploadID: "u-anon", OwnerIP: "10.0.0.1",
		}))
		_, err := repo.FirstByUploadIDOwnedBy(ctx, "u-anon", nil, "10.0.0.2")
		require.True(t, errors.Is(err, gorm.ErrRecordNotFound))
	})

	t.Run("匿名同 IP 命中", func(t *testing.T) {
		repo := newRepo(t)
		require.NoError(t, repo.Create(ctx, &model.FileCode{
			Code: "ANON0002", UploadID: "u-anon-same", OwnerIP: "10.0.0.9",
		}))
		fc, err := repo.FirstByUploadIDOwnedBy(ctx, "u-anon-same", nil, "10.0.0.9")
		require.NoError(t, err)
		assert.Equal(t, "ANON0002", fc.Code)
	})

	t.Run("负对照：空 IP 不与他人空 IP 互相命中", func(t *testing.T) {
		repo := newRepo(t)
		require.NoError(t, repo.Create(ctx, &model.FileCode{
			Code: "ANON0003", UploadID: "u-anon-empty", OwnerIP: "",
		}))
		_, err := repo.FirstByUploadIDOwnedBy(ctx, "u-anon-empty", nil, "")
		require.True(t, errors.Is(err, gorm.ErrRecordNotFound),
			"取不到 IP 的两个调用方不得互相命中（否则本修复被重新打穿）")
	})

	t.Run("同一 upload_id 多条时取自己的，而不是 id 最小的", func(t *testing.T) {
		repo := newRepo(t)
		// 先落受害者那条（id 更小）——旧实现 Order id ASC 必命中它
		require.NoError(t, repo.Create(ctx, &model.FileCode{
			Code: "EARLY001", UploadID: "u-shared", UserID: uid(1), OwnerIP: "10.0.0.1",
		}))
		require.NoError(t, repo.Create(ctx, &model.FileCode{
			Code: "LATER001", UploadID: "u-shared", UserID: uid(2), OwnerIP: "10.0.0.2",
		}))

		mine, err := repo.FirstByUploadIDOwnedBy(ctx, "u-shared", uid(2), "10.0.0.2")
		require.NoError(t, err)
		assert.Equal(t, "LATER001", mine.Code, "必须命中调用方自己的那条")

		victim, err := repo.FirstByUploadIDOwnedBy(ctx, "u-shared", uid(1), "10.0.0.1")
		require.NoError(t, err)
		assert.Equal(t, "EARLY001", victim.Code)

		// 旧行为对照：无限定回读仍取 id 最小（受害者那条）⇒ 证明判据确实换了
		legacy, err := repo.FirstByUploadID(ctx, "u-shared")
		require.NoError(t, err)
		assert.Equal(t, "EARLY001", legacy.Code)
	})

	t.Run("软删行不参与", func(t *testing.T) {
		repo := newRepo(t)
		fc := &model.FileCode{Code: "SOFT0001", UploadID: "u-soft", UserID: uid(3), OwnerIP: "10.0.0.3"}
		require.NoError(t, repo.Create(ctx, fc))
		require.NoError(t, repo.Delete(ctx, fc.ID))
		_, err := repo.FirstByUploadIDOwnedBy(ctx, "u-soft", uid(3), "10.0.0.3")
		require.True(t, errors.Is(err, gorm.ErrRecordNotFound))
	})

	t.Run("空 upload_id 直接未命中", func(t *testing.T) {
		repo := newRepo(t)
		_, err := repo.FirstByUploadIDOwnedBy(ctx, "", uid(1), "10.0.0.1")
		require.True(t, errors.Is(err, gorm.ErrRecordNotFound))
	})
}
