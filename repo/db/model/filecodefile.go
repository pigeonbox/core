package model

import (
	"path/filepath"
	"strings"

	"gorm.io/gorm"
)

// FileCodeFile 多文件分享的子文件行（P0 多文件：1 个 FileCode 分享 ↔ N 个文件）。
//
// 与主表的分工（2026-10-26 订正：旧注释说的是已退役的「目录+UUID 文件名」
// 双形态——该形态已统一为「完整相对路径」）：
//
//	· 主表保留首个文件的字段（FilePath/UUIDFileName/Size/FileHash），
//	  供管理端列表/秒传/统计读取；主表 Size = 各文件之和。
//	· **子表是文件清单的唯一真相源**（取件页展示/逐个下载/zip 打包都读它），
//	  每文件一行，FilePath 同样是「含唯一文件名的完整相对路径」。
type FileCodeFile struct {
	gorm.Model
	FileCodeID uint   `gorm:"index;not null" json:"file_code_id"`
	FileName   string `gorm:"size:255" json:"file_name"` // 原始文件名（消毒后，仅展示）
	// RelDir 分享内相对目录（5.3.0 目录上传）："a/b" 形态，空 = 位于根。
	//
	// 与 FilePath 的分工：FilePath 是**存储内部**路径（含 UUID 文件名、与用户
	// 目录无关，服务端自己造）；RelDir 是**用户视角**的目录，取件页按它还原
	// 目录树、打包下载按它保留结构。
	//
	// 为什么不把目录拼进 FileName：FileName 是「文件名」语义，同时被下载按钮的
	// 推荐文件名、列表排序、秒传展示名三处消费，混入目录会让这三处一起出问题。
	RelDir       string `gorm:"size:255" json:"rel_dir"`
	UUIDFileName string `gorm:"size:255" json:"uuid_file_name"` // 磁盘/对象唯一名
	FilePath     string `gorm:"size:255" json:"file_path"`      // 存储相对路径（含唯一文件名）
	Size         int64  `gorm:"default:0" json:"size"`
	FileHash     string `gorm:"size:64" json:"file_hash"` // SHA-256（秒传/完整性校验）
	SortOrder    int    `gorm:"default:0" json:"sort_order"`
}

// GetFullPath 取存储相对路径（与 FileCode.GetFilePath 不同：子行恒为新格式，
// 直接返回 FilePath）。
func (f *FileCodeFile) GetFullPath() string {
	return f.FilePath
}

// FullName 用户视角的完整相对路径（"a/b/file.txt"；位于根时即 DisplayName）。
// 取件页目录树与 zip 条目名都以它为口径——两处必须同一个来源，否则
// 「页面上看到的路径」与「解压出来的路径」会各自漂移。
func (f *FileCodeFile) FullName() string {
	name := f.DisplayName()
	dir := strings.Trim(strings.TrimSpace(f.RelDir), "/")
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// DisplayName 展示名：优先 FileName，回退 UUID 文件名/路径末段。
func (f *FileCodeFile) DisplayName() string {
	if name := strings.TrimSpace(f.FileName); name != "" {
		return name
	}
	if f.UUIDFileName != "" {
		return f.UUIDFileName
	}
	return filepath.Base(f.FilePath)
}
