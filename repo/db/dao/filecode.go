package dao

import (
	"context"
	"path/filepath"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

type FileCodeRepository struct {
	conn *gorm.DB // 构造注入（去全局化终态，见 doc.go）
}

// NewFileCodeRepository 构造 repository；gormDB 必填（去全局化终态，无全局回退），约定见 doc.go。
func NewFileCodeRepository(gormDB *gorm.DB) *FileCodeRepository {
	return &FileCodeRepository{conn: gormDB}
}

func (r *FileCodeRepository) db() *gorm.DB {
	return r.conn
}

func (r *FileCodeRepository) Create(ctx context.Context, fileCode *model.FileCode) error {
	return r.db().WithContext(ctx).Create(fileCode).Error
}

func (r *FileCodeRepository) GetByID(ctx context.Context, id uint) (*model.FileCode, error) {
	var fileCode model.FileCode
	err := r.db().WithContext(ctx).First(&fileCode, id).Error
	if err != nil {
		return nil, err
	}
	return &fileCode, nil
}

func (r *FileCodeRepository) GetByCode(ctx context.Context, code string) (*model.FileCode, error) {
	var fileCode model.FileCode
	err := r.db().WithContext(ctx).Where("code = ?", code).First(&fileCode).Error
	if err != nil {
		return nil, err
	}
	return &fileCode, nil
}

// FirstByUploadID 按上传会话 ID 查已建的分享（**幂等回读**用）。
//
// 背景（2026-10-24 审计，core v0.32.6）：上传完成面（chunk/presign 的 Complete）
// 此前**不检查是否已建过分享** ⇒ 同一 upload_id 二次 Complete（双击 / 客户端
// 重试 / 网络重试）会**再建一条分享**：两个分享各有独立的取件次数额度与取件码
// ⇒ owner 设的「最多取 N 次」实际可取 2N 次；用户存储统计也被双计。
// （真机实测：13 条带 upload_id 的分享里已有 **1 组重复** ⇒ 该缺陷已在生产
// 数据上发生。）
//
// ⇒ 完成面先按 upload_id 回读已有分享 ⇒ 命中即**幂等返回**（同一个分享码），
// 不新建行、不重复扣次、不重复计配额。
//
// 命中多条时取 **id 最小**（最早建的那条）⇒ 与 GetByCodeFolded 的确定性口径
// 一致；空 upload_id 不参与（普通分享没有上传会话）。
func (r *FileCodeRepository) FirstByUploadID(ctx context.Context, uploadID string) (*model.FileCode, error) {
	if uploadID == "" {
		return nil, gorm.ErrRecordNotFound
	}
	var fileCode model.FileCode
	err := r.db().WithContext(ctx).
		Where("upload_id = ? AND deleted_at IS NULL", uploadID).
		Order("id ASC").First(&fileCode).Error
	if err != nil {
		return nil, err
	}
	return &fileCode, nil
}

// GetByCodeFolded 大小写折叠检索（UPPER 归一；码表全 ASCII，sqlite/mysql/pg 通吃）。
// 调用方必须先精确 GetByCode 未命中再兜底本方法，热路径不恒走函数查询；
// 多行同码不同大小写时按 id 升序取最早一条，保证结果确定性。
func (r *FileCodeRepository) GetByCodeFolded(ctx context.Context, code string) (*model.FileCode, error) {
	var fileCode model.FileCode
	err := r.db().WithContext(ctx).
		Where("UPPER(code) = UPPER(?)", code).
		Order("id ASC").First(&fileCode).Error
	if err != nil {
		return nil, err
	}
	return &fileCode, nil
}

// FindQuickUploadSource 秒传源检索（2026-10-12 起限定同用户，D1=A 同用户内去重）：
// 仅匹配**调用者自己**的历史文件分享，命中"正常态 + 未过期"且有存储路径。
//
// 隐私边界（D1 拍板）：跨用户匹配是存在性 oracle——持候选文件者可撞库探测
// "本站是否有人传过该文件"，故仅限同用户（自己的文件自己知道，无泄露面）。
// 回归（2026-10-03）：不过滤 status 会把 blocked/待审分享当秒传源（假成功 UX +
// 违规内容借尸还魂）；回归二（2026-10-10）：候选按 id 降序 Go 侧挑最新可用，
// 防过期旧记录挡死后来全部有效候选。无可用候选返回 gorm.ErrRecordNotFound。
func (r *FileCodeRepository) FindQuickUploadSource(ctx context.Context, userID uint, fileHash string, size int64) (*model.FileCode, error) {
	if userID == 0 || fileHash == "" || size <= 0 {
		return nil, gorm.ErrRecordNotFound
	}
	var candidates []model.FileCode
	err := r.db().WithContext(ctx).
		Where("user_id = ? AND file_hash = ? AND size = ? AND deleted_at IS NULL AND status = ?",
			userID, fileHash, size, model.StatusNormal).
		Order("id DESC").
		Limit(32).
		Find(&candidates).Error
	if err != nil {
		return nil, err
	}
	for i := range candidates {
		if !candidates[i].IsExpired() && candidates[i].GetFilePath() != "" {
			return &candidates[i], nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

// CountFilePathReferences 统计引用某存储路径的分享行数（file_codes 主表 +
// file_code_files 子表，均含软删行——软删行的物理文件留存供回收站恢复/清理
// 任务处理）。excludeFileCodeID=本次要失效的分享自身 ID（0=不排）。
// 秒传去重后多个分享共享同一物理文件，物理删除前必须确认无其他引用
// （2026-10-12 秒传/去重）；查不清时调用方应跳过删除（宁泄漏勿误删）。
//
// 路径比对在 Go 侧按 GetFilePath() 解析后进行：file_codes 有新旧两种落库布局
// （整路径 / 目录+uuid 文件名），SQL 精确比对会漏后者——按 file_path 相等或
// uuid_file_name=basename 宽取候选，再解析归一比对。
func (r *FileCodeRepository) CountFilePathReferences(ctx context.Context, filePath string, excludeFileCodeID uint) (int64, error) {
	if filePath == "" {
		return 0, nil
	}
	var rows []model.FileCode
	if err := r.db().WithContext(ctx).Unscoped().
		Where("(file_path = ? OR uuid_file_name = ?) AND id <> ?",
			filePath, filepath.Base(filePath), excludeFileCodeID).
		Find(&rows).Error; err != nil {
		return 0, err
	}
	var n int64
	for i := range rows {
		if rows[i].GetFilePath() == filePath {
			n++
		}
	}
	var children []model.FileCodeFile
	if err := r.db().WithContext(ctx).Unscoped().
		Where("file_path = ? AND file_code_id <> ?", filePath, excludeFileCodeID).
		Find(&children).Error; err != nil {
		return 0, err
	}
	return n + int64(len(children)), nil
}

// GetByHashAndSize 已删除（2026-10-12）：全库秒传改为 FindQuickUploadSource
// 同用户语义（D1=A），旧全局匹配是存在性 oracle 不再提供。

func (r *FileCodeRepository) Update(ctx context.Context, fileCode *model.FileCode) error {
	return r.db().WithContext(ctx).Save(fileCode).Error
}

func (r *FileCodeRepository) UpdateColumns(ctx context.Context, id uint, updates map[string]interface{}) error {
	return r.db().WithContext(ctx).Model(&model.FileCode{}).Where("id = ?", id).Updates(updates).Error
}

func (r *FileCodeRepository) Delete(ctx context.Context, id uint) error {
	return r.db().WithContext(ctx).Delete(&model.FileCode{}, id).Error
}

func (r *FileCodeRepository) DeleteByFileCode(ctx context.Context, fileCode *model.FileCode) error {
	return r.db().WithContext(ctx).Delete(fileCode).Error
}

func (r *FileCodeRepository) Count(ctx context.Context) (int64, error) {
	var count int64
	err := r.db().WithContext(ctx).Model(&model.FileCode{}).Count(&count).Error
	return count, err
}

// CountToday 已删除：全库无调用方（统计走 CountTodayUploads/CountCreatedBetween）。

func (r *FileCodeRepository) CountActive(ctx context.Context) (int64, error) {
	var count int64
	err := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Where("expired_at IS NULL OR expired_at > ? OR expired_count > 0", time.Now()).
		Count(&count).Error
	return count, err
}

func (r *FileCodeRepository) GetTotalSize(ctx context.Context) (int64, error) {
	var totalSize int64
	err := r.db().WithContext(ctx).Model(&model.FileCode{}).Select("COALESCE(SUM(size), 0)").Scan(&totalSize).Error
	return totalSize, err
}

func (r *FileCodeRepository) CountTodayUploads(ctx context.Context) (int64, error) {
	var count int64
	today := time.Now().Format("2006-01-02")
	err := r.db().WithContext(ctx).Model(&model.FileCode{}).Where("created_at >= ?", today).Count(&count).Error
	return count, err
}

func (r *FileCodeRepository) List(ctx context.Context, page, pageSize int, search string) ([]*model.FileCode, int64, error) {
	page, pageSize = clampPage(page, pageSize, MaxPageSize)

	query := r.db().WithContext(ctx).Model(&model.FileCode{})

	// 搜索条件（pickup_code 纳入 LIKE 链，对齐用户面/管理端 ListWithFilter——
	// 2026-10-09 遗留缺口：管理面老 List 搜 6 位取件码不命中；NULL 列 LIKE 恒不匹配，无副作用）
	if search != "" {
		pattern, esc := LikeContains(search)
		query = query.Where("code LIKE ? OR prefix LIKE ? OR suffix LIKE ? OR pickup_code LIKE ?"+esc,
			pattern, pattern, pattern, pattern)
	}

	return paginate[model.FileCode](query.Order("created_at DESC"), page, pageSize)
}

func (r *FileCodeRepository) GetExpiredFiles(ctx context.Context) ([]*model.FileCode, error) {
	var expiredFiles []*model.FileCode
	err := r.db().WithContext(ctx).Where("(expired_at IS NOT NULL AND expired_at < ?) OR expired_count = 0", time.Now()).
		Find(&expiredFiles).Error
	return expiredFiles, err
}

func (r *FileCodeRepository) DeleteExpiredFiles(ctx context.Context, expiredFiles []*model.FileCode) (int, error) {
	if len(expiredFiles) == 0 {
		return 0, nil
	}

	count := 0
	for _, file := range expiredFiles {
		if err := r.db().WithContext(ctx).Delete(file).Error; err != nil {
			continue // 记录错误但继续处理其他文件
		}
		count++
	}
	return count, nil
}
