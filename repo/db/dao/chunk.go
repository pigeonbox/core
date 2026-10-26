package dao

import (
	"context"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

type ChunkRepository struct {
	conn *gorm.DB // 构造注入（去全局化终态，见 doc.go）
}

// NewChunkRepository 构造 repository；gormDB 必填（去全局化终态，无全局回退），约定见 doc.go。
func NewChunkRepository(gormDB *gorm.DB) *ChunkRepository {
	return &ChunkRepository{conn: gormDB}
}

func (r *ChunkRepository) db() *gorm.DB {
	return r.conn
}

func (r *ChunkRepository) Create(ctx context.Context, chunk *model.UploadChunk) error {
	return r.db().WithContext(ctx).Create(chunk).Error
}

func (r *ChunkRepository) GetByHash(ctx context.Context, chunkHash string, fileSize int64) (*model.UploadChunk, error) {
	var chunk model.UploadChunk
	err := r.db().WithContext(ctx).Where("chunk_hash = ? AND file_size = ? AND chunk_index = -1", chunkHash, fileSize).First(&chunk).Error
	if err != nil {
		return nil, err
	}
	return &chunk, nil
}

func (r *ChunkRepository) GetByUploadID(ctx context.Context, uploadID string) (*model.UploadChunk, error) {
	var chunk model.UploadChunk
	err := r.db().WithContext(ctx).Where("upload_id = ? AND chunk_index = -1", uploadID).First(&chunk).Error
	if err != nil {
		return nil, err
	}
	return &chunk, nil
}

// GetByUploadIDUnscoped 查**含软删**的控制行（chunk_index=-1）。
//
// 用途（2026-10-26 实录修复）：upload_id 由 file_hash 派生（客户端不带
// upload_id 时服务端用哈希当会话键）⇒ **同一文件重传会复用同一 upload_id**；
// 而会话清理（DeleteStaleSessions）对控制行做的是**软删** ⇒ 默认作用域的
// GetByUploadID 查不到 ⇒ init 若走「新建」会撞 (upload_id, chunk_index) 唯一索引
// （迁移 000006 的索引**不区分软删行**）⇒ 直接 10008「初始化上传失败」。
// ⇒ 需要能查到软删行并**复活**它（见 ReviveSession）。
func (r *ChunkRepository) GetByUploadIDUnscoped(ctx context.Context, uploadID string) (*model.UploadChunk, error) {
	var chunk model.UploadChunk
	err := r.db().WithContext(ctx).Unscoped().
		Where("upload_id = ? AND chunk_index = -1", uploadID).
		Order("deleted_at DESC, id DESC").First(&chunk).Error
	if err != nil {
		return nil, err
	}
	return &chunk, nil
}

// ReviveSession 复活被软删的会话：**控制行 + 分片行**一起复活，但
// 分片行的完成标记**置回未完成**。
//
// 为什么要一起复活分片行：分片写入走 UpsertChunk（UPDATE ... WHERE
// upload_id=? AND chunk_index=?）⇒ 默认作用域**排除软删行** ⇒ 只复活控制行的话，
// 分片行仍是软删态 ⇒ 上传分片「更新不到任何行」⇒ 分片永远记不上 ⇒
// Complete 合并失败（分片数不足）。
//
// 为什么要把分片行的 completed 置回 false：清理窗口过后磁盘上的分片文件可能已被
// 回收 ⇒ 若仍按旧进度宣称「已传 N 片」⇒ 客户端跳过重传 ⇒ Complete 时找不到分片
// 文件 ⇒ 合并失败。置回未完成后客户端会重传，语义与新会话一致。
//
// 幂等：行不存在时不影响其它行（UPDATE 命中 0 行不报错）。
func (r *ChunkRepository) ReviveSession(ctx context.Context, uploadID string) error {
	// ⚠️ 两条 UPDATE 必须各自**新建语句**：复用同一个 tx 会让 gorm 把前一条的
	// WHERE 累加进后一条 ⇒ 变成 `(chunk_index = -1) AND (chunk_index >= 0)`
	// ⇒ 永远命中 0 行（实测踩过：控制行复活了、分片行没复活 ⇒ 分片记不上）。
	// ⚠️ deleted_at 用 gorm.Expr("NULL") 显式落库：Updates(map) 里的 nil 值
	// 会被 gorm 跳过（实测：行仍是软删态 ⇒ 复活静默失败）。
	if err := r.db().WithContext(ctx).Unscoped().
		Model(&model.UploadChunk{}).
		Where("upload_id = ? AND chunk_index = -1", uploadID).
		Updates(map[string]interface{}{
			"deleted_at":  gorm.Expr("NULL"),
			"status":      "pending",
			"completed":   false,
			"retry_count": 0,
		}).Error; err != nil {
		return err
	}
	return r.db().WithContext(ctx).Unscoped().
		Model(&model.UploadChunk{}).
		Where("upload_id = ? AND chunk_index >= 0", uploadID).
		Updates(map[string]interface{}{
			"deleted_at": gorm.Expr("NULL"),
			"status":     "pending",
			"completed":  false,
		}).Error
}

func (r *ChunkRepository) GetChunkByIndex(ctx context.Context, uploadID string, chunkIndex int) (*model.UploadChunk, error) {
	var chunk model.UploadChunk
	err := r.db().WithContext(ctx).Where("upload_id = ? AND chunk_index = ? AND completed = true", uploadID, chunkIndex).First(&chunk).Error
	if err != nil {
		return nil, err
	}
	return &chunk, nil
}

func (r *ChunkRepository) UpdateUploadProgress(ctx context.Context, uploadID string, completedChunks int) error {
	return r.db().WithContext(ctx).Model(&model.UploadChunk{}).
		Where("upload_id = ? AND chunk_index = -1", uploadID).
		Updates(map[string]interface{}{
			"completed":   completedChunks,
			"retry_count": gorm.Expr("retry_count + 1"),
		}).Error
}

func (r *ChunkRepository) UpdateChunkCompleted(ctx context.Context, uploadID string, chunkIndex int, chunkHash string) error {
	// Model 必须显式指定：仅 Where+Updates(map) 时 GORM 无从解析表名，
	// 恒报 "Table not set"（回归：分片完成端点 CompleteUpload 因此恒 500）
	return r.db().WithContext(ctx).Model(&model.UploadChunk{}).
		Where("upload_id = ? AND chunk_index = ?", uploadID, chunkIndex).
		Updates(map[string]interface{}{
			"completed":  true,
			"chunk_hash": chunkHash,
			"status":     "completed",
		}).Error
}

func (r *ChunkRepository) CountCompletedChunks(ctx context.Context, uploadID string) (int64, error) {
	var count int64
	err := r.db().WithContext(ctx).Model(&model.UploadChunk{}).
		Where("upload_id = ? AND chunk_index >= 0 AND completed = true", uploadID).
		Count(&count).Error
	return count, err
}

func (r *ChunkRepository) DeleteByUploadID(ctx context.Context, uploadID string) error {
	return r.db().WithContext(ctx).Where("upload_id = ?", uploadID).Delete(&model.UploadChunk{}).Error
}

func (r *ChunkRepository) GetUploadList(ctx context.Context, page, pageSize int) ([]*model.UploadChunk, int64, error) {
	var chunks []*model.UploadChunk
	var total int64

	// 只获取控制记录（chunk_index = -1）
	query := r.db().WithContext(ctx).Model(&model.UploadChunk{}).Where("chunk_index = -1")

	// 获取总数
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	// 分页查询
	offset := (page - 1) * pageSize
	err := query.Order("created_at DESC").Offset(offset).Limit(pageSize).Find(&chunks).Error

	return chunks, total, err
}

// GetIncompleteUploads 超过 maxAge 未完成的控制记录（chunk_index=-1）。
// cutoff 由 Go 侧计算后传参——修复：此前用 SQLite 专属的
// datetime('now', '-' || ? || ' hours')，MySQL/Postgres 部署直接 SQL 报错。
func (r *ChunkRepository) GetIncompleteUploads(ctx context.Context, maxAge time.Duration) ([]*model.UploadChunk, error) {
	cutoff := time.Now().Add(-maxAge)
	var chunks []*model.UploadChunk
	err := r.db().WithContext(ctx).
		Where("chunk_index = -1 AND status != 'completed' AND updated_at < ?", cutoff).
		Find(&chunks).Error
	return chunks, err
}

// DeleteStaleSessions 删除过期会话的全部行（控制记录 + 分片行）。
// completed 为 true 时清理已完成会话的旧行（控制表防膨胀）。
func (r *ChunkRepository) DeleteStaleSessions(ctx context.Context, maxAge time.Duration, completed bool) (int64, error) {
	cutoff := time.Now().Add(-maxAge)
	q := r.db().WithContext(ctx).Where("updated_at < ?", cutoff)
	if completed {
		q = q.Where("chunk_index = -1 AND status = 'completed'")
	} else {
		q = q.Where("chunk_index = -1 AND status != 'completed'")
	}
	var controls []*model.UploadChunk
	if err := q.Find(&controls).Error; err != nil {
		return 0, err
	}
	if len(controls) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(controls))
	for _, c := range controls {
		ids = append(ids, c.UploadID)
	}
	// 空 ids 直接返回：GORM 对「空 IN 列表 + Delete」的组合会抛
	// 「unsupported data type: Table not set」⇒ 清理器把错误当失败上抛。
	if len(ids) == 0 {
		return 0, nil
	}
	res := r.db().WithContext(ctx).Model(&model.UploadChunk{}).
		Where("upload_id IN ?", ids).Delete(&model.UploadChunk{})
	return res.RowsAffected, res.Error
}

func (r *ChunkRepository) GetOldChunks(ctx context.Context, cutoffTime time.Time) ([]*model.UploadChunk, error) {
	var oldChunks []*model.UploadChunk
	err := r.db().WithContext(ctx).Where("created_at < ? AND chunk_index = -1", cutoffTime).Find(&oldChunks).Error
	return oldChunks, err
}

func (r *ChunkRepository) DeleteChunksByUploadIDs(ctx context.Context, uploadIDs []string) (int, error) {
	if len(uploadIDs) == 0 {
		return 0, nil
	}

	count := 0
	for _, uploadID := range uploadIDs {
		if err := r.db().WithContext(ctx).Where("upload_id = ?", uploadID).Delete(&model.UploadChunk{}).Error; err != nil {
			continue // 记录错误但继续处理其他上传
		}
		count++
	}
	return count, nil
}

// ListChunksUnscoped 列出某会话的**全部**分片行（含软删）。
// 运维/诊断用：核对复活后的会话里分片行的真实状态（活/软删/完成标记）。
func (r *ChunkRepository) ListChunksUnscoped(ctx context.Context, uploadID string) ([]*model.UploadChunk, error) {
	var rows []*model.UploadChunk
	err := r.db().WithContext(ctx).Unscoped().
		Where("upload_id = ? AND chunk_index >= 0", uploadID).
		Order("chunk_index ASC").Find(&rows).Error
	return rows, err
}

// PurgeSoftDeletedChunks **硬删除**软删超过 maxAge 的会话行（控制行 + 分片行）。
//
// 为什么需要（2026-10-26）：DeleteStaleSessions 对会话行是**软删**（软删语义被
// 「会话复活」依赖：同 upload_id 重来时要能查到旧行并复活）⇒ upload_chunks 表
// **只增不减** ⇒ 长期运行后软删行堆积、把表撑大。
//
// 窗口取 30 天：软删行在窗口内仍**占用 (upload_id, chunk_index) 唯一键** ⇒ 复活
// 路径正是靠这一点把「同一文件重传」接回来；硬清理后键释放，新建路径也能走通
// （复活与新建两条路径互补）。
func (r *ChunkRepository) PurgeSoftDeletedChunks(ctx context.Context, maxAge time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxAge)
	// 硬删除（Unscoped）：**必须带 Model()** —— 只给 Dest 值时 GORM 解析不到表
	// ⇒ 抛「unsupported data type: Table not set」；而 janitor 侧只 Warn
	// ⇒ 硬清理会**静默从未执行**（2026-10-26 由单测逮住）。
	res := r.db().WithContext(ctx).Unscoped().
		Model(&model.UploadChunk{}).
		Where("deleted_at IS NOT NULL AND deleted_at < ?", cutoff).
		Delete(&model.UploadChunk{})
	return res.RowsAffected, res.Error
}

func (r *ChunkRepository) GetUploadedChunkIndexes(ctx context.Context, uploadID string) ([]int, error) {
	var uploadedChunks []int
	err := r.db().WithContext(ctx).Model(&model.UploadChunk{}).
		Where("upload_id = ? AND completed = true AND chunk_index >= 0", uploadID).
		Pluck("chunk_index", &uploadedChunks).Error
	return uploadedChunks, err
}

func (r *ChunkRepository) FirstOrCreateChunk(ctx context.Context, chunk *model.UploadChunk) error {
	return r.db().WithContext(ctx).Where("upload_id = ? AND chunk_index = ?", chunk.UploadID, chunk.ChunkIndex).
		Assign(chunk).
		FirstOrCreate(chunk).Error
}

// UpsertChunk 按 (upload_id, chunk_index) **幂等写入**分片行：存在则更新其
// 内容字段，不存在则插入。
//
// ⚠️ 为什么不用 FirstOrCreateChunk（2026-10-25 审计 A3）：gorm 的
// `FirstOrCreate` 命中既有行时**不套用 Assign**（Assign 只在 create 分支生效）
// ⇒ 已存在的未完成行（completed=false）**不会被更新为已完成** ⇒ 重传仍不生效。
//
// 为什么需要它：`GetChunkByIndex` 的 WHERE 含 `completed = true` ⇒ 看不到未完成
// 的旧行 ⇒ 修前「失败后重传同索引」会**再建一行** ⇒ CountCompletedChunks 按
// **行数**计 ⇒ 「分片齐全」判定可虚高（声明 5 片只传 4 片也可能计成 5）。
//
// 并发：两个请求同时「未命中 ⇒ 插入」时，后者会撞 (upload_id, chunk_index)
// 唯一索引（迁移 000006）⇒ 捕获唯一冲突后退回更新（幂等语义不破）。
func (r *ChunkRepository) UpsertChunk(ctx context.Context, chunk *model.UploadChunk) error {
	fields := map[string]interface{}{
		"chunk_hash": chunk.ChunkHash,
		"chunk_size": chunk.ChunkSize,
		"status":     chunk.Status,
		"completed":  chunk.Completed,
	}
	res := r.db().WithContext(ctx).Model(&model.UploadChunk{}).
		Where("upload_id = ? AND chunk_index = ?", chunk.UploadID, chunk.ChunkIndex).
		Updates(fields)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}
	// 未命中 ⇒ 插入；并发下可能撞唯一索引 ⇒ 退回更新
	if err := r.db().WithContext(ctx).Create(chunk).Error; err != nil {
		if isUniqueViolation(err) {
			return r.db().WithContext(ctx).Model(&model.UploadChunk{}).
				Where("upload_id = ? AND chunk_index = ?", chunk.UploadID, chunk.ChunkIndex).
				Updates(fields).Error
		}
		return err
	}
	return nil
}

// UpdateOwner 更新会话归属字段（IP 漂移后的会话接管）
func (r *ChunkRepository) UpdateOwner(ctx context.Context, uploadID string, updates map[string]interface{}) error {
	return r.db().WithContext(ctx).Model(&model.UploadChunk{}).
		Where("upload_id = ? AND chunk_index = -1", uploadID).
		Updates(updates).Error
}

// ListSessionIDs 全量分片会话 ID（含软删，Distinct）——物理 chunks/ 目录对账用。
func (r *ChunkRepository) ListSessionIDs(ctx context.Context) ([]string, error) {
	var ids []string
	if err := r.db().WithContext(ctx).Unscoped().
		Model(&model.UploadChunk{}).Distinct().Pluck("upload_id", &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}
