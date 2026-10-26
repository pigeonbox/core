// suffix.go — 扩展名推导（仪表盘「文件类型分布」用）。
//
// ⚠️ 2026-10-26：本文件源于一次兼容层退役的漏网残留——管理台统计曾直接查
// **已退役的 suffix 列**（该列恒空 ⇒ widget 早就是死的；退役后变成「SQL 报错被
// `_` 吞掉」⇒ 恒空且**零日志**）。⇒ 改为从**现役列**现算扩展名。
//
// 口径与 model.FileCode.DisplayName() 对齐：文件分享的**原始文件名**在 `text`
// （直传/分片/预签名/本地导入四个通道都写它）⇒ 扩展名 = 最后一个点之后的尾巴。
package model

import "strings"

// FileExtension 取文件名的扩展名（**含前导点**，如 ".pdf"）。
//
// 形态约定：与被退役的 suffix 列的旧形态一致（前端 `suffix.replace(/^\./, ”)`
// 照常工作）⇒ 仪表盘的「文件类型分布」不用改前端。
//
// 无扩展名（"README"、"archive."）⇒ 返回空串（调用方跳过）。
// 含路径（"a/b/c.pdf"）⇒ 先取末段再判扩展名。
func FileExtension(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	// 末段（兼容 '/' 与 '\' 两种分隔符）
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	dot := strings.LastIndex(name, ".")
	// 无点 / 点在开头（".gitignore" 视为无扩展名）/ 点在末尾 ⇒ 无扩展名
	if dot <= 0 || dot == len(name)-1 {
		return ""
	}
	return name[dot:]
}
