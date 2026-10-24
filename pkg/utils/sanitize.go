package utils

import (
	"github.com/oekit/pkg/pathx"
)

// maxFileNameLength 文件名最大长度（含扩展名，单位**字节**）。
// 覆盖常见文件系统 255 字节上限的一半，兼顾 UTF-8 多字节字符。
const maxFileNameLength = 120

// SanitizeFileName 消毒用户提供的原始文件名（展示/存储用）：
//   - 剥离目录成分（/ \ ..），防止路径穿越
//   - 丢弃控制字符，Windows 保留字符 : * ? " < > | 换下划线
//   - 去首尾空白与结尾的点（Windows 目录项陷阱）
//   - 空名/纯符号名回退为 "file"
//   - 超长按字节截断且保留扩展名
//
// 返回值仅用于"展示/存储原始名"，磁盘文件名仍由 UUID 生成，双保险。
//
// 实现委托 oekit/pkg/pathx.SanitizeFileName（口径见该包文档）；本仓只额外
// 钉死长度上限（120 字节）——磁盘侧按字节卡，多字节文件名不会落盘 ENAMETOOLONG。
func SanitizeFileName(name string) string {
	return pathx.SanitizeFileName(name, pathx.WithMaxFileNameLength(maxFileNameLength))
}
