package utils

import (
	"errors"
	"testing"

	"github.com/pigeonbox/core/conf"
	"github.com/stretchr/testify/assert"
)

// withUploadConfig 为单个测试注入上传配置，结束后恢复原全局配置。
func withUploadConfig(t *testing.T, u conf.UploadConfig) {
	t.Helper()
	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{Upload: u})
	t.Cleanup(func() {
		conf.SetGlobalConfig(old)
	})
}

func TestIsBlockedExtension_Blocked(t *testing.T) {
	bl := DefaultBlockedExtensions()
	assert.True(t, IsBlockedExtension("malware.exe", bl))
	assert.True(t, IsBlockedExtension("script.SH", bl)) // 大小写不敏感
	assert.True(t, IsBlockedExtension("a.b.bat", bl))
	assert.True(t, IsBlockedExtension("scr.scr", bl))
}

func TestIsBlockedExtension_Allowed(t *testing.T) {
	bl := DefaultBlockedExtensions()
	assert.False(t, IsBlockedExtension("photo.jpg", bl))
	assert.False(t, IsBlockedExtension("doc.pdf", bl))
	assert.False(t, IsBlockedExtension("noext", bl))
	assert.False(t, IsBlockedExtension("archive.tar.gz", bl))
}

// ---- 治理重构（2026-10-03）：白名单/黑名单/魔数组合语义 ----

func TestIsAllowedExtension_Semantics(t *testing.T) {
	t.Run("未初始化配置", func(t *testing.T) {
		withUploadConfig(t, conf.UploadConfig{})
		assert.True(t, IsAllowedExtension("photo.jpg")) // 非黑名单 → 允许
		assert.False(t, IsAllowedExtension("app.exe"))  // 默认黑名单 → 拒绝
	})
	t.Run("白名单空=黑名单模式", func(t *testing.T) {
		withUploadConfig(t, conf.UploadConfig{AllowedExtensions: nil})
		assert.True(t, IsAllowedExtension("doc.pdf"))
		assert.False(t, IsAllowedExtension("a.exe"))
	})
	t.Run("白名单非空=必须命中", func(t *testing.T) {
		withUploadConfig(t, conf.UploadConfig{AllowedExtensions: []string{".jpg", ".png"}})
		assert.True(t, IsAllowedExtension("photo.JPG")) // 大小写不敏感
		assert.False(t, IsAllowedExtension("a.exe"))    // 未命中 → 拒绝
		assert.False(t, IsAllowedExtension("README"))   // 无扩展名也按未命中 → 拒绝
	})
	t.Run("白名单命中但黑名单仍拒绝", func(t *testing.T) {
		withUploadConfig(t, conf.UploadConfig{AllowedExtensions: []string{".exe"}})
		assert.False(t, IsAllowedExtension("a.exe"))
	})
}

func TestGetBlockedExtensions(t *testing.T) {
	t.Run("配置空回退默认", func(t *testing.T) {
		withUploadConfig(t, conf.UploadConfig{})
		bl := GetBlockedExtensions()
		assert.Contains(t, bl, ".exe")
	})
	t.Run("配置非空覆盖默认", func(t *testing.T) {
		withUploadConfig(t, conf.UploadConfig{BlockedExtensions: []string{".tgz"}})
		bl := GetBlockedExtensions()
		assert.Equal(t, []string{".tgz"}, bl)
	})
}

func TestGetEnableMagicCheck(t *testing.T) {
	t.Run("未初始化默认开", func(t *testing.T) {
		old := conf.GetGlobalConfig()
		conf.SetGlobalConfig(nil)
		t.Cleanup(func() { conf.SetGlobalConfig(old) })
		assert.True(t, GetEnableMagicCheck())
	})
	t.Run("显式配置", func(t *testing.T) {
		withUploadConfig(t, conf.UploadConfig{EnableMagicCheck: false})
		assert.False(t, GetEnableMagicCheck())
		withUploadConfig(t, conf.UploadConfig{EnableMagicCheck: true})
		assert.True(t, GetEnableMagicCheck())
	})
}

func TestCheckUploadContent_Orchestration(t *testing.T) {
	jpgHead := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	exeHead := []byte("MZ\x90\x00")

	cases := []struct {
		name    string
		file    string
		allowed []string
		blocked []string
		magic   bool
		head    []byte
		wantErr error
		wantAs  error
	}{
		{"干净文件放行", "a.jpg", nil, nil, true, jpgHead, nil, nil},
		{"白名单未命中拒绝", "a.exe", []string{".jpg"}, nil, true, nil, ErrFileTypeNotAllowed, nil},
		{"黑名单可配置覆盖默认", "a.tgz", nil, []string{".tgz"}, true, nil, ErrFileTypeNotAllowed, nil},
		{"白名单命中仍查黑名单", "a.exe", []string{".exe"}, nil, true, nil, ErrFileTypeNotAllowed, nil},
		{"魔数命中拒绝", "a.jpg", nil, nil, true, exeHead, nil, &MagicMismatchError{}},
		{"魔数开关关闭放行", "a.jpg", nil, nil, false, exeHead, nil, nil},
		{"白名单未命中优先于魔数", "a.png", []string{".jpg"}, nil, true, nil, ErrFileTypeNotAllowed, nil},
		{"黑名单空回退默认", "a.bat", nil, nil, true, nil, ErrFileTypeNotAllowed, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withUploadConfig(t, conf.UploadConfig{
				AllowedExtensions: tc.allowed,
				BlockedExtensions: tc.blocked,
				EnableMagicCheck:  tc.magic,
			})
			err := CheckUploadContent(tc.file, tc.head)
			if tc.wantErr != nil {
				assert.True(t, errors.Is(err, tc.wantErr), "want %v, got %v", tc.wantErr, err)
			} else if tc.wantAs != nil {
				var mme *MagicMismatchError
				assert.True(t, errors.As(err, &mme), "want as %T, got %v", tc.wantAs, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
