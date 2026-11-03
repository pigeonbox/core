// 魔数豁免名单语义守卫（2026-11-03，真机缺陷驱动）。
//
// 缺陷实录：同事传 ZCode 的 .AppImage（AppImage 按规范就是 ELF）被魔数检查
// 误杀——1.35GB 传完才在 presign Complete 报「文件内容与扩展名不符（检测到
// ELF (linux executable)）」，业务码还塌成 10008「内部错误」。
//
// 语义判据＝**是否构成伪装**：
//   - 诚实扩展名（.appimage/.run/.bin/…、无扩展名）+ 可执行魔数 ⇒ 放行；
//   - 文档/图片扩展名 + 可执行魔数（cat.jpg 实为 PE）⇒ **必须仍然拒绝**。
//
// 断言必须能变红（负对照）：把 magicCheckSkipExts 清空（旧实现）⇒
// 诚实扩展名用例组整体变红；把魔数命中分支删掉 ⇒ 伪装用例组变红。
package utils

import (
	"errors"
	"testing"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/conf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	elfHead  = []byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01}
	mzHead   = []byte("MZ\x90\x00\x03")
	shebang  = []byte("#!/bin/sh\n")
	cafeBabe = []byte{0xca, 0xfe, 0xba, 0xbe}
)

// TestMagicSkip_HonestExtensions 诚实扩展名 + 可执行魔数 ⇒ 放行。
// 负对照：清空 magicCheckSkipExts（回到旧实现）⇒ 本组全部变红。
func TestMagicSkip_HonestExtensions(t *testing.T) {
	cases := []struct {
		file string
		head []byte
	}{
		// 真机误杀场景本尊：AppImage ＝ ELF
		{"ZCode-3.15.3-linux-x64.AppImage", elfHead},
		{"zcode.appimage", elfHead},
		// 无扩展名：不作类型声明（共享编译产物，如 pigeonbox-linux-amd64）
		{"pigeonbox-linux-amd64", elfHead},
		// makeself 自解压安装器：#! 脚本或 ELF
		{"NVIDIA-Linux-x86_64-550.run", shebang},
		{"setup.run", elfHead},
		// 通用二进制/固件块与 ELF 载体
		{"firmware.bin", mzHead},
		{"vmlinux.elf", elfHead},
		{"shell.efi", elfHead},
		{"wireguard.ko", elfHead},
		{"a.out", elfHead},
		// 脚本扩展名：#! 属本义
		{"deploy.py", shebang},
		{"task.rb", shebang},
		{"tool.pl", shebang},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			assert.NoError(t, CheckUploadContent(tc.file, tc.head),
				"%s 的内容是诚实声明的可执行/脚本格式，不应被魔数检查误杀", tc.file)
		})
	}
}

// TestMagicSkip_DisguiseStillRejected 伪装必须仍然被拦（安全不变量）。
// 负对照：删掉魔数命中分支 ⇒ 本组全部变红。
func TestMagicSkip_DisguiseStillRejected(t *testing.T) {
	cases := []struct {
		file string
		head []byte
	}{
		{"cat.jpg", elfHead},     // ELF 伪装图片
		{"report.pdf", mzHead},   // PE 伪装文档
		{"notes.txt", shebang},   // 脚本伪装文本
		{"icon.png", cafeBabe},   // Java class 伪装图片
		{"invoice.docx", mzHead}, // PE 伪装文档
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			var mme *MagicMismatchError
			require.True(t, errors.As(CheckUploadContent(tc.file, tc.head), &mme),
				"%s 是可执行内容伪装，必须被魔数检查拒绝", tc.file)
		})
	}
}

// TestMagicSkip_BeatsOnlyMagic_BlockedExtStillWins 扩展名黑名单永远优先：
// 管理员把 .appimage 加进黑名单 ⇒ 即便魔数豁免也必须拒（禁传能力不受影响）。
func TestMagicSkip_BeatsOnlyMagic_BlockedExtStillWins(t *testing.T) {
	withUploadConfig(t, conf.UploadConfig{BlockedExtensions: []string{".appimage"}})
	assert.ErrorIs(t, CheckUploadContent("x.AppImage", elfHead), ErrFileTypeNotAllowed,
		"黑名单命中必须优先于魔数豁免")
}

// TestMagicSkip_ConfigExtension 配置 magic_check_skip_extensions 只做**追加**：
// 站点特有扩展名可豁免，内置豁免不受配置影响。
func TestMagicSkip_ConfigExtension(t *testing.T) {
	// 未配置：.mybin 不在豁免名单 ⇒ ELF 被拒（对照）
	withUploadConfig(t, conf.UploadConfig{EnableMagicCheck: true})
	var mme *MagicMismatchError
	require.True(t, errors.As(CheckUploadContent("tool.mybin", elfHead), &mme), "对照：未配置时应拒")

	// 配置追加 ⇒ 放行
	withUploadConfig(t, conf.UploadConfig{EnableMagicCheck: true, MagicCheckSkipExts: []string{".MyBin"}})
	assert.NoError(t, CheckUploadContent("tool.mybin", elfHead), "配置追加豁免应放行（大小写不敏感）")

	// 内置豁免不受影响
	assert.NoError(t, CheckUploadContent("x.appimage", elfHead), "内置豁免恒生效")
}

// TestFileTypeErrors_CarryBusinessCode typed error 契约：类型/魔数拒绝必须带
// 30011 业务码 ⇒ handler 的 default 分支经 resp.NewTypedError 透传时落
// 415/30011，而不是 10008「内部错误」（真机实录：用户只看到「内部错误」）。
func TestFileTypeErrors_CarryBusinessCode(t *testing.T) {
	mme := &MagicMismatchError{Signature: "ELF (linux executable)"}
	var coder interface{ ErrCode() int }
	require.True(t, errors.As(mme, &coder))
	assert.Equal(t, errcode.CodeFileTypeDenied, coder.ErrCode())

	require.True(t, errors.As(ErrFileTypeNotAllowed, &coder), "ErrFileTypeNotAllowed 应是 typed error")
	assert.Equal(t, errcode.CodeFileTypeDenied, coder.ErrCode())
}
