// uploadpath.go 上传相对路径的统一生成收口。
//
// 随机码/令牌生成已上收至 github.com/oekit/kitx/uidgen（2026-10-05 起）；
// 上传相对路径 uploads/YYYY/MM/DD/ 的拼接逻辑保留在此。
package utils

import (
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// NewUploadRelPath 上传相对路径 uploads/YYYY/MM/DD/<uuid><ext>。
// 单次取时钟构造日期段（避免跨秒边界日期漂移）；返回 (唯一文件名, 相对路径)。
func NewUploadRelPath(originalName string) (uuidName, rel string) {
	ext := filepath.Ext(originalName)
	uuidName = uuid.New().String() + ext
	now := time.Now()
	rel = filepath.Join("uploads", now.Format("2006"), now.Format("01"), now.Format("02"), uuidName)
	return uuidName, rel
}
