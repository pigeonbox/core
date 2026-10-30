// files.go 文件/分享治理簇：列表/详情/恢复/彻底删除/管控状态/删除/延期与批量操作。
package admin

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/repo/db/model"
)

// GetFiles 获取文件列表
func (s *Service) GetFiles(ctx context.Context, page, pageSize int, search string) ([]*model.FileCode, int64, error) {
	return s.fileCodeRepo.List(ctx, page, pageSize, search)
}

// GetFilesFiltered 管理端文件列表组合过滤（治理 2026-10-03：
// keyword/user/upload_type/owner_ip/status/大小/时间/过期，供 /admin/files/filter）。
//
// 2026-10-29：顺带批量补齐上传者用户名 —— 此前只给裸 user_id，管理台「上传者」
// 列只能渲染 "ID: 1"，管理员看不出是谁传的。补齐失败**不阻断**列表返回
// （用户名是展示增强，不是查询前提）。
func (s *Service) GetFilesFiltered(ctx context.Context, q model.FileCodeQuery) ([]*model.FileCode, int64, error) {
	files, total, err := s.fileCodeRepo.ListWithFilter(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	s.fillUploaderNames(ctx, files)
	return files, total, nil
}

// fillUploaderNames 批量补上传者用户名（一次 IN 查询，无 N+1）。
//
// 不走 JOIN 的原因：file_codes 与 users 同名列（created_at/status/updated_at/
// deleted_at）⇒ JOIN 后既有的 Where 条件会撞「ambiguous column name」。
// 账号已删/匿名 ⇒ 用户名留空，由前端回退显示 ID / 匿名。
func (s *Service) fillUploaderNames(ctx context.Context, files []*model.FileCode) {
	if len(files) == 0 || s.userRepo == nil {
		return
	}
	ids := make([]uint, 0, len(files))
	seen := make(map[uint]bool, len(files))
	for _, f := range files {
		if f.UserID == nil || *f.UserID == 0 || seen[*f.UserID] {
			continue
		}
		seen[*f.UserID] = true
		ids = append(ids, *f.UserID)
	}
	if len(ids) == 0 {
		return
	}
	names, err := s.userRepo.UsernamesByIDs(ctx, ids)
	if err != nil {
		logger.CtxWarn(ctx, "fill uploader names failed", "err", err)
		return
	}
	for _, f := range files {
		if f.UserID == nil {
			continue
		}
		f.Username = names[*f.UserID]
	}
}

// RestoreFiles 从回收站恢复（软删 → 存活）：deleted_at 置空，COS 对象未动无需处理。
// 仅恢复"当前处于软删态"的 id，忽略其余；返回实际恢复数。
func (s *Service) RestoreFiles(ctx context.Context, ids []uint) (int64, error) {
	n, err := s.fileCodeRepo.RestoreByIDs(ctx, ids)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.logAdminOperation(ctx, "file.restore",
			fmt.Sprintf("ids=%v restored from recycle bin", ids), true)
	}
	return n, nil
}

// PurgeFiles 彻底删除（回收站 → 物理删除）：DB 行硬删 + COS/本地对象删除。
// 对象删除失败不阻断 DB 硬删（孤儿由桶生命周期/对账兜底）；返回成功数。
func (s *Service) PurgeFiles(ctx context.Context, ids []uint) (int64, error) {
	var purged int64
	for _, id := range ids {
		file, err := s.fileCodeRepo.GetByIDUnscoped(ctx, id)
		if err != nil {
			continue
		}
		// 秒传共享对象保护（2026-10-12）：删前查引用，被其他分享共享时保留物理文件
		//
		// ⚠️ 2026-10-26 审计修复：原实现只删**主表那一个**对象 ⇒ 多文件分享的
		// 非首个文件（只在 file_code_files 子表里）在硬删 DB 行后**彻底失去引用**
		// ⇒ 本地 24h 后被对账清，远端**永久残留**。⇒ 遍历子行逐个删
		// （与 BatchDeleteFiles 同口径）。
		if fp := file.GetFilePath(); fp != "" {
			s.deletePhysicalIfUnreferenced(ctx, fp, id)
		}
		if children, cerr := s.fileFileRepo.ListByFileCodeID(ctx, id); cerr == nil {
			for _, c := range children {
				if c.FilePath != "" {
					s.deletePhysicalIfUnreferenced(ctx, c.FilePath, id)
				}
			}
		}
		if _, err := s.fileCodeRepo.HardDeleteByIDs(ctx, []uint{id}); err != nil {
			logger.CtxWarn(ctx, "purge db row failed", "id", id, "err", err)
			continue
		}
		purged++
		s.logAdminOperation(ctx, "file.purge",
			fmt.Sprintf("file %d (code=%s) purged permanently", id, file.Code), true)
	}
	return purged, nil
}

// SetFilesStatus 管理员设置分享管控状态（单个/批量禁用、恢复共用）。
// 返回受影响行数；审计按动作分别落账（file.block / file.unblock / file.status）。
func (s *Service) SetFilesStatus(ctx context.Context, ids []uint, status string) (int64, error) {
	n, err := s.fileCodeRepo.UpdateStatusByIDs(ctx, ids, status)
	if err != nil {
		s.logAdminOperation(ctx, "file.status",
			fmt.Sprintf("set %v status=%s failed: %v", ids, status, err), false)
		return 0, err
	}
	action := "file.status"
	switch status {
	case model.StatusBlocked:
		action = "file.block"
	case model.StatusNormal:
		action = "file.unblock"
	}
	s.logAdminOperation(ctx, action,
		fmt.Sprintf("ids=%v status=%s affected=%d", ids, status, n), true)
	return n, nil
}

// DeleteFile 删除文件（DB 记录 + 物理文件；此前物理删除被注释，造成存储泄漏）
func (s *Service) DeleteFile(ctx context.Context, fileID uint) error {
	file, err := s.fileCodeRepo.GetByID(ctx, fileID)
	if err != nil {
		return err
	}

	// 1. 删除数据库记录
	if err := s.fileCodeRepo.Delete(ctx, fileID); err != nil {
		return err
	}

	// 2. 删除物理文件（失败记日志不阻断——DB 已删，孤儿文件由清理任务兜底；
	//    秒传共享对象保护：删前查引用，被其他分享共享时保留）
	if s.storage != nil && file.FilePath != "" {
		s.deletePhysicalIfUnreferenced(ctx, file.GetFilePath(), fileID)
	}

	// 3. 联邦撤销公告（未启用为 no-op；快照读——热切换并发安全）
	if fed := s.currentFederation(); fed != nil {
		fed.ShareDeleted(file.Code)
	}

	s.logAdminOperation(ctx, "file.delete",
		fmt.Sprintf("file %d (code=%s, name=%s) deleted", fileID, file.Code, file.Text), true)
	return nil
}

// deletePhysicalIfUnreferenced 物理删除保护（2026-10-12 秒传去重共享对象）：
// 无其他分享引用 filePath 时才删；查不清跳过删除（宁泄漏勿误删，janitor 对账兜底）。
// excludeFileCodeID=本次要失效的分享自身 ID（0=不排）。
func (s *Service) deletePhysicalIfUnreferenced(ctx context.Context, filePath string, excludeFileCodeID uint) {
	if s.storage == nil || filePath == "" {
		return
	}
	n, err := s.fileCodeRepo.CountFilePathReferences(ctx, filePath, excludeFileCodeID)
	if err != nil {
		logger.CtxWarn(ctx, "physical delete skipped: reference count failed (leak, janitor reconciles)",
			"path", filePath, "err", err)
		return
	}
	if n > 0 {
		return // 仍被其他分享引用（秒传共享对象），保留物理文件
	}
	if err := s.storage.DeleteFile(ctx, filePath); err != nil {
		logger.CtxWarn(ctx, "delete physical file failed", "path", filePath, "err", err)
	}
}

// GetFileByCode 按码获取分享记录（**分享码或 6 位取件码**都认）。
//
// 此前只查 code 列（分享码/自定义码）⇒ 管理员手里只有取件码时查不到
// （与 MCP 的 resolveFileCode 结论相反 ⇒ 同一输入两个管理面结论相左）。
// ⇒ 统一走 dao.ResolveByCode 解析器。
func (s *Service) GetFileByCode(ctx context.Context, code string) (*model.FileCode, error) {
	return s.fileCodeRepo.ResolveByCode(ctx, code, conf.CodeFoldEnabledOrDefault())
}

// GetFileByID 按主键取分享记录（管理端详情/下载重定向）。
func (s *Service) GetFileByID(ctx context.Context, id uint) (*model.FileCode, error) {
	return s.fileCodeRepo.GetByID(ctx, id)
}

// GetFileDetail 文件详情：主记录 + 子文件行。
// （2026-10-26 订正：旧注释说「旧单文件分享无子行由调用方回退主表合成」——
//
//	虚拟行已退役、**没有**这条回退路径；子表是文件清单的唯一真相源，
//	存量无子行的分享由启动期数据迁移补齐。）
func (s *Service) GetFileDetail(ctx context.Context, id uint) (*model.FileCode, []*model.FileCodeFile, error) {
	fc, err := s.fileCodeRepo.GetByID(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	children, err := s.fileFileRepo.ListByFileCodeID(ctx, fc.ID)
	if err != nil {
		return fc, nil, err
	}
	return fc, children, nil
}

// ChildFileCounts 批量取子文件数（一次 GROUP BY；失败返回 nil 由调用方降级为 0）。
func (s *Service) ChildFileCounts(ctx context.Context, ids []uint) map[uint]int64 {
	counts, err := s.fileFileRepo.CountByFileCodeIDs(ctx, ids)
	if err != nil {
		return nil
	}
	return counts
}

// UpdateFileExpire 编辑文件（延期 / 改剩余次数）。
func (s *Service) UpdateFileExpire(ctx context.Context, id uint, expireAt *time.Time, expiredCount *int) error {
	return s.fileCodeRepo.UpdateExpireByID(ctx, id, expireAt, expiredCount)
}

// BatchDeleteFiles 批量删除：DB 软删 + 物理文件清理（best-effort，失败不阻断，
// 孤儿由 janitor 对账）。返回受影响行数。
func (s *Service) BatchDeleteFiles(ctx context.Context, ids []uint) (int, error) {
	// 先取口令供联邦撤销（批量接口不回报实际命中，按请求集合撤销；
	// 误撤未删码由联邦客户端心跳补公告自愈；快照读——热切换并发安全）
	codes := make([]string, 0, len(ids))
	fed := s.currentFederation()
	if fed != nil {
		for _, id := range ids {
			if fc, err := s.fileCodeRepo.GetByID(ctx, id); err == nil {
				codes = append(codes, fc.Code)
			}
		}
	}

	// 物理文件清理（含多文件子文件）。此前纯 DB 删除：本地后端靠每日对账
	// 兜底，远端后端（s3/webdav/多云）上即永久孤儿对象——2026-10-06 215
	// COS 实测确认。失败不阻断 DB 删除（记日志）。
	childRepo := s.fileFileRepo
	for _, id := range ids {
		fc, err := s.fileCodeRepo.GetByID(ctx, id)
		if err != nil {
			continue
		}
		if s.storage != nil {
			if children, cerr := childRepo.ListByFileCodeID(ctx, id); cerr == nil {
				for _, c := range children {
					if c.FilePath == "" {
						continue
					}
					// 秒传共享对象保护（2026-10-12）：删前查引用
					s.deletePhysicalIfUnreferenced(ctx, c.FilePath, id)
				}
				if len(children) > 0 {
					_ = childRepo.SoftDeleteByFileCodeIDs(ctx, []uint{id})
				}
			}
		}
		if s.storage != nil && fc.FilePath != "" {
			s.deletePhysicalIfUnreferenced(ctx, fc.GetFilePath(), id)
		}
	}

	n, err := s.fileCodeRepo.BatchDeleteByIDs(ctx, ids)
	if err == nil && fed != nil {
		for _, code := range codes {
			fed.ShareDeleted(code)
		}
	}
	return n, err
}

// BatchExtendFiles 管理端跨用户批量延期。
func (s *Service) BatchExtendFiles(ctx context.Context, ids []uint, expireAt time.Time) (int, error) {
	return s.fileCodeRepo.BatchExtendByIDsAdmin(ctx, ids, expireAt)
}

// ListReferencedObjectPaths 列出被 DB 引用的存储对象路径（存储迁移用）。
//
// 与孤儿对账（janitor.ReconcileOrphans）的引用集口径**必须一致**：
//
//	· 主表 file_codes（软删也含：软删分享可恢复 ⇒ 对象要一起搬）；
//	· 子表 file_code_files（多文件分享的非首文件只在这里）；
//	· 路径归一（斜杠 + Clean）后去重（同一路径可能被多行引用）。
//
// 差异：孤儿对账要的是"哪些**不是**孤儿"，这里要的是"哪些**要搬**"——
// 引用集相同，用途不同。
func (s *Service) ListReferencedObjectPaths(ctx context.Context) ([]string, error) {
	if s == nil || s.fileCodeRepo == nil {
		return nil, fmt.Errorf("存储对象清单来源未就绪")
	}
	seen := make(map[string]bool)
	out := make([]string, 0, 64)

	rows, err := s.fileCodeRepo.ListAllIncludingDeleted(ctx)
	if err != nil {
		return nil, fmt.Errorf("列举分享记录失败: %w", err)
	}
	for _, r := range rows {
		if p := normalizeObjectPath(r.GetFilePath()); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	childPaths, err := s.fileFileRepo.ListAllPathsIncludingDeleted(ctx)
	if err != nil {
		return nil, fmt.Errorf("列举多文件清单失败: %w", err)
	}
	for _, p := range childPaths {
		if p := normalizeObjectPath(p); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}

// normalizeObjectPath 归一对象路径（斜杠 + Clean）；空路径返回空串。
func normalizeObjectPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(p))
}
