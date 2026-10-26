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
	FileCodeID   uint   `gorm:"index;not null" json:"file_code_id"`
	FileName     string `gorm:"size:255" json:"file_name"`      // 原始文件名（消毒后，仅展示）
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
