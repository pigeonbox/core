// blobref.go 匿名占位通道对文件生命周期账本（blobs/file_references）的接入
// （core#172 拆分项 3·匿名直建面 Reference 化）。
//
// # 为什么本域要单独一份
//
// 匿名通道（/anonymous/generate → CreateAnonymousShare 直建 file_codes 行、
// presign 取件码绑定 → BindFilePath 回填对象）**不经 share.CreateShare** ⇒
// 首片挂在分享域的 attachBlobRef 覆盖不到这些行；不补挂则释放/回收时账本侧
// 查不到引用（双守卫的账本侧失明）。
//
// # 取舍（与 app/share/blobref.go 同口径）
//
//   - **best-effort（失败仅告警，不阻断主链）**：账本缺行是泄漏方向——物理
//     对象仍由既有 CountFilePathReferences 路径口径与 janitor 对账兜底，
//     泄漏方向、不误删；反过来因账本失败拒绝出码/回填是把新面的可用性风险
//     倒灌进主链，不可取。
//   - **占位期不挂**：FilePath 为空（生产 /anonymous/generate 形态）没有物理
//     对象，attach 自跳过；对象落定由 BindFilePath 补挂。
//   - 软删/恢复不碰账本（释放只挂硬删，见 share/blobref.go；本域行的硬删
//     释放走 #207 的硬删全链）。
package anonymous

import (
	"context"

	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/repo/db/model"
)

// attachBlobRef 直建/回填 file_codes 行后的账本双写（best-effort）：
// objectKey 空（占位/文本分享）直接跳过；失败仅告警（见文件头取舍）。
func (s *Service) attachBlobRef(ctx context.Context, fc *model.FileCode) {
	if s.blobRepo == nil || fc == nil {
		return
	}
	key := fc.GetFilePath()
	if key == "" {
		return // 占位行尚无物理对象（BindFilePath 回填时再挂）
	}
	if _, err := s.blobRepo.EnsureBlobWithRef(ctx, key, fc.Size, fc.FileHash, fc.ID); err != nil {
		logger.CtxWarn(ctx, "blob ref attach failed (anonymous share still created; path-based reclaim still guards)",
			"code", fc.Code, "path", key, "err", err)
	}
}
