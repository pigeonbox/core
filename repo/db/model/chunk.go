package model

import (
	"time"

	"gorm.io/gorm"
)

// UploadChunk 上传分片模型
type UploadChunk struct {
	gorm.Model

	// UploadID 与 ChunkIndex 组成**复合唯一键**（2026-10-25 审计 A3）：
	// 无此约束时，分片失败后重传同索引会**再建一行**（dao.GetChunkByIndex 只查
	// completed=true 的行 ⇒ 看不到未完成的旧行）⇒ CountCompletedChunks 按行数计
	// ⇒ 「分片齐全」判定可虚高（声明 5 片只传 4 片也可能计成 5）。
	//
	// ⚠️ **两个字段必须挂同一个索引名**才是复合索引：只给 ChunkIndex 挂
	// `uniqueIndex:` ⇒ gorm 建的是 **chunk_index 单列**唯一索引 ⇒ **不同上传
	// 会话的同索引分片互相冲突**（CI 真机冒烟实测：S13e 同用户秒传
	// 「UNIQUE constraint failed: upload_chunks.chunk_index」⇒ 整条秒传链路挂）。
	//
	// 生产走版本化迁移 000006（复合）；此处标签让**新建库**（AutoMigrate）也有。
	UploadID    string `gorm:"index;size:36;uniqueIndex:idx_upload_chunks_upload_id_chunk_index" json:"upload_id"`
	ChunkIndex  int    `gorm:"uniqueIndex:idx_upload_chunks_upload_id_chunk_index" json:"chunk_index"`
	ChunkHash   string `gorm:"size:64" json:"chunk_hash"`
	TotalChunks int    `json:"total_chunks"`
	FileSize    int64  `json:"file_size"`
	ChunkSize   int    `json:"chunk_size"`
	FileName    string `gorm:"size:255" json:"file_name"`

	// 归属追踪（治理 2026-10-03）：分片通道此前不记录上传者，
	// Complete 处置（滥用定位/归属校验）无从下手。控制记录（chunk_index=-1）填充。
	OwnerIP string `gorm:"size:45" json:"owner_ip"`
	UserID  *uint  `gorm:"index" json:"user_id"`

	Completed  bool   `gorm:"default:false" json:"completed"`
	RetryCount int    `gorm:"default:0" json:"retry_count"`            // 重试次数
	LastError  string `gorm:"type:text" json:"last_error"`             // 最后错误信息
	Status     string `gorm:"size:20;default:'pending'" json:"status"` // pending, uploading, completed, failed
}

// ChunkQuery 分片查询条件
type ChunkQuery struct {
	UploadID   string `json:"upload_id"`
	ChunkIndex *int   `json:"chunk_index"`
	Status     string `json:"status"`
	Completed  *bool  `json:"completed"`
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
}

// ChunkUpdate 分片更新数据
type ChunkUpdate struct {
	gorm.Model
	ChunkHash  *string    `json:"chunk_hash"`
	Completed  *bool      `json:"completed"`
	RetryCount *int       `json:"retry_count"`
	LastError  *string    `json:"last_error"`
	Status     *string    `json:"status"`
	UpdatedAt  *time.Time `json:"updated_at"`
}

// ChunkStats 分片统计查询结果
type ChunkStats struct {
	gorm.Model
	TotalChunks     int64 `json:"total_chunks"`
	CompletedChunks int64 `json:"completed_chunks"`
	PendingChunks   int64 `json:"pending_chunks"`
	FailedChunks    int64 `json:"failed_chunks"`
}
