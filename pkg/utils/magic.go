package utils

import (
	"bytes"
	"strings"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/pkg/metrics"
)

// magicSignature 内容魔数签名（前缀匹配）。
// 对标上游 2.5.x 的魔数校验：扩展名可以伪造，可执行格式的内容头不能。
var magicSignatures = []struct {
	name string
	pfx  []byte
}{
	{"Windows PE (exe/dll/sys)", []byte("MZ")},
	{"ELF (linux executable)", []byte{0x7f, 'E', 'L', 'F'}},
	{"Mach-O (macOS executable)", []byte{0xfe, 0xed, 0xfa, 0xce}},
	{"Mach-O 64-bit", []byte{0xfe, 0xed, 0xfa, 0xcf}},
	{"Mach-O (byte-swapped)", []byte{0xce, 0xfa, 0xed, 0xfe}},
	{"Mach-O 64-bit (byte-swapped)", []byte{0xcf, 0xfa, 0xed, 0xfe}},
	{"shell script", []byte("#!")},
	{"Java class", []byte{0xca, 0xfe, 0xba, 0xbe}},
	{"Windows shortcut", []byte{0x4c, 0x00, 0x00, 0x00, 0x01, 0x14, 0x02, 0x00}},
}

// magicCheckSkipExts 这些"扩展名对可执行/脚本内容是诚实声明"的扩展名跳过魔数检查。
//
// 判据＝**是否构成伪装**：魔数检查防的是"内容与扩展名不符"（cat.jpg 实为 PE），
// 而下列扩展名本身就声明了可执行/脚本/二进制内容，不存在伪装：
//   - ""（无扩展名）：不作任何类型声明；
//   - .appimage：AppImage 封包按规范就是 ELF（type3 ET_DYN）——2026-11-03
//     真机实录：同事传 ZCode 的 .AppImage 被误杀，传完 1.35GB 才在 Complete
//     报「文件内容与扩展名不符（检测到 ELF）」，业务码还塌成 10008 内部错误；
//   - .run：makeself 自解压安装器（#! 脚本或 ELF）；.bin/.elf/.efi/.ko/
//     .out/.bundle：二进制/驱动/EFI/链接产物，内容即本义。
//
// 硬拦截仍由扩展名黑名单承担且**优先于**本名单（.exe/.sh 等永远拒；管理员
// 想禁传 .appimage 时把它加进 upload.blocked_extensions 即可）。站点特有
// 扩展名经 upload.magic_check_skip_extensions **追加**（本名单是事实性
// 豁免，不提供移除能力——移除只会恢复误杀）。
var magicCheckSkipExts = map[string]bool{
	"":          true, // 无扩展名：不作类型声明
	".appimage": true, // AppImage ＝ ELF（type3 ET_DYN）
	".run":      true, // makeself 自解压：#! 脚本或 ELF
	".bin":      true, // 通用二进制块
	".elf":      true,
	".efi":      true, // EFI 应用：PE 或 ELF
	".ko":       true, // Linux 内核模块：ELF
	".out":      true, // a.out 链接产物：ELF
	".bundle":   true, // Mach-O 可加载束
	".py":       true, // 脚本扩展名：#! 属本义（同 .rb/.pl/.php/.lua/.tcl/.zsh 等）
	".rb":       true,
	".pl":       true,
	".php":      true,
	".lua":      true,
	".tcl":      true,
	".zsh":      true,
	".fish":     true,
	".ksh":      true,
	".csh":      true,
}

// MatchBlockedMagic 检查内容头部是否命中可执行文件魔数。
// head 为文件（或首个分片）的前若干字节。
func MatchBlockedMagic(head []byte) (string, bool) {
	for _, sig := range magicSignatures {
		if bytes.HasPrefix(head, sig.pfx) {
			return sig.name, true
		}
	}
	return "", false
}

// CheckUploadContent 组合校验：扩展名准入（白/黑名单）+ 魔数。
//   - IsAllowedExtension（白名单未命中 / 黑名单命中）→ 拒绝
//   - 魔数命中（开关 upload.enable_magic_check 开、head 非空、扩展名不在 skip 列表）→ 拒绝
//     防"jpg.exe 改名 cat.jpg"
//
// head 可为 nil（调用方拿不到内容头时只做扩展名检查）。
func CheckUploadContent(filename string, head []byte) error {
	if !IsAllowedExtension(filename) {
		metrics.RecordRejected(metrics.RejectType)
		return ErrFileTypeNotAllowed
	}
	if len(head) > 0 && GetEnableMagicCheck() {
		ext := strings.ToLower(filenameExt(filename))
		if !isMagicCheckSkipExt(ext) {
			if name, hit := MatchBlockedMagic(head); hit {
				metrics.RecordRejected(metrics.RejectType)
				return &MagicMismatchError{Signature: name}
			}
		}
	}
	return nil
}

// isMagicCheckSkipExt 生效豁免判定：内置名单 + upload.magic_check_skip_extensions
// 追加（站点特有扩展名，配置侧大小写不敏感）。
func isMagicCheckSkipExt(ext string) bool {
	if magicCheckSkipExts[ext] {
		return true
	}
	for _, e := range GetMagicCheckSkipExts() {
		if strings.ToLower(e) == ext {
			return true
		}
	}
	return false
}

// MagicMismatchError 扩展名与内容魔数不符
type MagicMismatchError struct {
	Signature string
}

func (e *MagicMismatchError) Error() string {
	return "文件内容与扩展名不符（检测到 " + e.Signature + "）"
}

// ErrCode 实现 app 侧 typed error 约定 ⇒ wire 上落 30011（HTTP 415）。
// 2026-11-03 实录：presign Complete 的 default 分支曾把它塌成 10008
// 「内部错误」——用户被误杀后只看到「内部错误」，无从判断是内容问题。
func (e *MagicMismatchError) ErrCode() int { return errcode.CodeFileTypeDenied }

func filenameExt(name string) string {
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 {
		return ""
	}
	return name[dot:]
}
