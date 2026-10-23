package codex

import (
	"context"
	"errors"
	"testing"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRepo 按「码 → 记录」的三种查法打桩（未命中返回 gorm.ErrRecordNotFound 语义）。
type fakeRepo struct {
	byCode        map[string]*model.FileCode
	byFolded      map[string]*model.FileCode
	byPickup      map[string]*model.FileCode
	pickupQueried []string
	codeQueried   []string
}

func (f *fakeRepo) GetByCode(ctx context.Context, code string) (*model.FileCode, error) {
	f.codeQueried = append(f.codeQueried, code)
	if fc, ok := f.byCode[code]; ok {
		return fc, nil
	}
	return nil, errors.New("record not found")
}

func (f *fakeRepo) GetByCodeFolded(ctx context.Context, code string) (*model.FileCode, error) {
	if fc, ok := f.byFolded[code]; ok {
		return fc, nil
	}
	return nil, errors.New("record not found")
}

func (f *fakeRepo) GetByPickupCode(ctx context.Context, pickupCode string) (*model.FileCode, error) {
	f.pickupQueried = append(f.pickupQueried, pickupCode)
	if fc, ok := f.byPickup[pickupCode]; ok {
		return fc, nil
	}
	return nil, errors.New("record not found")
}

// 按码解析是**全站唯一真相源**（分享域/预览域/匿名域/MCP 四家共用）⇒ 它的
// 优先级与归一口径一旦漂移，四个面会各说各话。
func TestResolve_Priority(t *testing.T) {
	ctx := context.Background()
	share := &model.FileCode{Code: "AbCd1234"}
	pickup := &model.FileCode{Code: "XyZ0987", PickupCode: strPtr("WS9WHP")}
	repo := &fakeRepo{
		byCode:   map[string]*model.FileCode{"AbCd1234": share},
		byFolded: map[string]*model.FileCode{"abcd1234": share},
		byPickup: map[string]*model.FileCode{"WS9WHP": pickup},
	}

	t.Run("8 位分享码命中 code 列", func(t *testing.T) {
		fc, err := Resolve(ctx, repo, "AbCd1234", true)
		require.NoError(t, err)
		assert.Equal(t, "AbCd1234", fc.Code)
	})

	t.Run("大小写折叠兜底", func(t *testing.T) {
		fc, err := Resolve(ctx, repo, "abcd1234", true)
		require.NoError(t, err)
		assert.Equal(t, "AbCd1234", fc.Code)
	})

	t.Run("6 位取件码命中取件码列（并归一为大写）", func(t *testing.T) {
		fc, err := Resolve(ctx, repo, "ws9whp", true)
		require.NoError(t, err)
		assert.Equal(t, "XyZ0987", fc.Code)
		assert.Equal(t, "WS9WHP", repo.pickupQueried[len(repo.pickupQueried)-1])
	})

	t.Run("首尾空白容忍（分享码与取件码同口径）", func(t *testing.T) {
		fc, err := Resolve(ctx, repo, "  AbCd1234  ", true)
		require.NoError(t, err)
		assert.Equal(t, "AbCd1234", fc.Code)
		// code 列也用 trim 后的串（MCP 旧实现会 TrimSpace ⇒ 保留该行为）
		assert.Equal(t, "AbCd1234", repo.codeQueried[len(repo.codeQueried)-1])
	})

	t.Run("非 6 位输入不查取件码列（省一次索引查询）", func(t *testing.T) {
		before := len(repo.pickupQueried)
		_, _ = Resolve(ctx, repo, "AbCd1234", true)
		assert.Equal(t, before, len(repo.pickupQueried))
	})

	t.Run("全查不到 ⇒ 报错", func(t *testing.T) {
		_, err := Resolve(ctx, repo, "ZZZZZZ", true)
		assert.Error(t, err)
	})
}

// 占位判定（未完成上传）必须只有一处 —— 分散在多域会各自漂移。
func TestIsNotReady(t *testing.T) {
	// 占位：既无文件路径、也无文本
	assert.True(t, IsNotReady(&model.FileCode{}))
	// 文本分享：Text 非空 ⇒ 不是占位
	assert.False(t, IsNotReady(&model.FileCode{Text: "hello"}))
	// 文件分享：有文件路径 ⇒ 不是占位
	assert.False(t, IsNotReady(&model.FileCode{FilePath: "uploads/x/a.bin"}))
	// nil ⇒ 不判为占位（避免把"没查到"误报成"未上传"）
	assert.False(t, IsNotReady(nil))
}

func strPtr(s string) *string { return &s }
