package chunk

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/pigeonbox/core/pkg/gate"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
	"gorm.io/gorm"
)

type InitiateUploadReq struct {
	UploadID     string
	FileName     string
	TotalChunks  int
	FileSize     int64
	ChunkSize    int
	OwnerIP      string // 上传者 IP（治理归属追踪）
	UserID       *uint  // 上传者（chunk 路由当前无可选认证，恒 nil；字段就位以便接线）
	SessionToken string // 客户端回传的会话令牌（X-Upload-Token；IP 漂移后恢复会话用）
}

// ErrSessionConflict init 撞上他人进行中（已含分片数据）的会话。
// 2026-10-05 审计 P1：uploadID 可为客户端自报哈希（公开文件哈希可预测），
// 原"最后写入者 wins"无条件重绑归属让第三方可截胡他人上传——改归属后即可
// 绑走已传分片（内容窃取）或覆写合并结果（内容替换）。HTTP 层映射 409。
var ErrSessionConflict = errors.New("上传会话已被占用，请刷新上传")

// MaxTotalChunks 单会话分片数硬上限（2026-10-08 加固）。TotalChunks 客户端
// 可控且此前无上限：一次 init 可声明上亿分片刷控制行，后续每个 chunk PUT
// 还会按 index 逐条建行。10000 片 × 常见 4MB 分片已承载 40GB，远超常规
// upload.max_file_size 配置，正常客户端不可能触顶。
const MaxTotalChunks = 10000

// ValidateChunkPlan 校验分片计划自洽性：TotalChunks 必须恰为 ⌈FileSize/ChunkSize⌉
// 且不超过 MaxTotalChunks。防两类滥用：① 总数虚高（配合逐片 PUT 刷 DB 行/
// 存储写）；② 总数不足让 Complete 永不满足的僵尸会话占库。自家前端按
// Math.ceil(file.size/chunkSize) 计算，严格相等不误伤。
func ValidateChunkPlan(fileSize, chunkSize int64, totalChunks int) error {
	if fileSize <= 0 || chunkSize <= 0 {
		return errors.New("文件大小与分片大小必须大于0")
	}
	expect := (fileSize + chunkSize - 1) / chunkSize
	if totalChunks <= 0 || int64(totalChunks) != expect {
		return fmt.Errorf("分片计划不自洽: total_chunks=%d, 期望 %d（⌈file_size/chunk_size⌉）", totalChunks, expect)
	}
	if totalChunks > MaxTotalChunks {
		return fmt.Errorf("分片数超过上限 %d", MaxTotalChunks)
	}
	return nil
}

type UploadChunkReq struct {
	UploadID   string
	ChunkIndex int
	ChunkHash  string
	ChunkSize  int
}

type ChunkResp struct {
	ID          uint   `json:"id"`
	UploadID    string `json:"upload_id"`
	ChunkIndex  int    `json:"chunk_index"`
	ChunkHash   string `json:"chunk_hash"`
	TotalChunks int    `json:"total_chunks"`
	FileSize    int64  `json:"file_size"`
	ChunkSize   int    `json:"chunk_size"`
	FileName    string `json:"file_name"`
	Completed   bool   `json:"completed"`
	Status      string `json:"status"`
}

type ProgressResp struct {
	UploadID        string  `json:"upload_id"`
	TotalChunks     int     `json:"total_chunks"`
	CompletedChunks int64   `json:"completed_chunks"`
	Progress        float64 `json:"progress"`
	Status          string  `json:"status"`
}

type Service struct {
	chunkRepo    *dao.ChunkRepository
	fileCodeRepo *dao.FileCodeRepository

	// 统一存储实例（bootstrap 注入；未注入时 storageClient 懒加载本地兜底，见 storage.go）
	storage         storage.StorageInterface
	fallbackOnce    sync.Once
	fallbackStorage storage.StorageInterface

	// 登录用户单次上传大小上限查询（bootstrap 注入 user 域；nil = 跳过预检）
	uploadCap UploadSizeCapProvider
}

// UploadSizeCapProvider 登录用户单次上传大小上限（字节，0 = 不限）。
// user.Service 以同签名满足本接口，不产生 chunk→user 域依赖。
type UploadSizeCapProvider interface {
	GetUploadSizeCap(ctx context.Context, userID uint) int64
}

// SetUploadSizeCap 注入上限查询方（bootstrap 接线）。
func (s *Service) SetUploadSizeCap(p UploadSizeCapProvider) { s.uploadCap = p }

func NewService(gormDB *gorm.DB) *Service {
	return &Service{
		chunkRepo:    dao.NewChunkRepository(gormDB),
		fileCodeRepo: dao.NewFileCodeRepository(gormDB),
	}
}

// InitiateUpload 初始化分片上传
func (s *Service) InitiateUpload(ctx context.Context, req *InitiateUploadReq) (*ChunkResp, error) {
	// 类型 + 整文件大小校验（应用层）。
	// 回归：此前误用 GetMaxUploadSize（单请求体上限 10MB）拦截整个文件，
	// 分片通道被单请求限制误伤。整文件上限走 upload.max_file_size（0=不限）。
	if err := utils.CheckWholeFileSize(req.FileSize); err != nil {
		// typed error（带 CodeTooLarge 业务码）⇒ handler 透传 413，
		// 而非落 500「初始化上传失败」（见 utils.FileTooLargeError 注释）
		return nil, utils.NewFileTooLargeError(utils.GetMaxFileSize())
	}
	if !utils.IsAllowedExtension(req.FileName) {
		return nil, fmt.Errorf("该文件类型禁止上传")
	}

	// 登录用户单次上传大小上限预检（fail-fast 于分片传输之前；2026-10-19 事故：
	// 450MB 分片全部传完在 multi-bind 才被隐藏的 50MB 上限拒绝，白费整轮传输）。
	// 匿名用户不受此限（走匿名日配额，bind/complete 侧把关）。
	if req.UserID != nil && s.uploadCap != nil && req.FileSize > 0 {
		capSize := s.uploadCap.GetUploadSizeCap(ctx, *req.UserID)
		if err := gate.CheckUploadSizeCap(req.UserID, req.FileSize, capSize); err != nil {
			return nil, err
		}
	}

	// 幂等语义：同 uploadID 重复 init（同哈希重传 / 断点重连）返回既有进度，
	// 由客户端经 status 续传——此前直接报错，秒传未命中时同哈希重传必 500。
	existing, err := s.chunkRepo.GetByUploadID(ctx, req.UploadID)
	if err == nil && existing != nil {
		// 会话接管收紧（2026-10-05 审计 P1，替代无条件"最后写入者 wins"）。
		// 放行仅三类：① 上传者本人（IP 或登录用户一致，断点重连语义不变）；
		// ② 持有效会话令牌（init 响应头 X-Upload-Token 签发，IP 漂移恢复通道）；
		// ③ 尚无任何分片落盘的空会话（孤儿回收——无内容可窃，保留原恢复语义）。
		// 已含数据的他人会话拒绝（ErrSessionConflict → 409）。
		if chunkSessionOwnedBy(existing, req.OwnerIP, req.UserID, req.SessionToken) ||
			!s.sessionHasChunks(ctx, existing.UploadID) {
			// 幂等语义：同 uploadID 重复 init（同哈希重传 / 断点重连）返回既有进度，
			// 由客户端经 status 续传——此前直接报错，秒传未命中时同哈希重传必 500。
			if existing.Status != "completed" && req.OwnerIP != "" && existing.OwnerIP != req.OwnerIP {
				updates := map[string]interface{}{"owner_ip": req.OwnerIP}
				if req.UserID != nil {
					updates["user_id"] = *req.UserID
				}
				if err := s.chunkRepo.UpdateOwner(ctx, existing.UploadID, updates); err == nil {
					existing.OwnerIP = req.OwnerIP
				}
			}
			return controlToResp(existing), nil
		}
		return nil, ErrSessionConflict
	}

	// 旧会话已被清理（软删）后的同一 upload_id 重来 ⇒ **复活**而不是新建。
	//
	// 事故实录（2026-10-26 真机冒烟 S11c/S11d 假红→真缺陷）：upload_id 由
	// file_hash 派生（客户端不带 upload_id 时服务端拿哈希当会话键）⇒ 同一文件
	// 重传会复用同一 upload_id；会话清理（DeleteStaleSessions）对控制行是
	// **软删** ⇒ 上面 GetByUploadID（默认排除软删行）查不到 ⇒ 走「新建」⇒
	// 撞 (upload_id, chunk_index) 唯一索引（迁移 000006 的索引**不区分软删行**）
	// ⇒ init 直接 10008「初始化上传失败」⇒ **同一文件在清理窗口过后必然无法
	// 分片上传**（真机 6 个软删控制行 = 6 个「死」文件）。
	//
	// 修法：查到软删的控制行 ⇒ 归属校验同 live 分支 ⇒ 复活它（进度归零 ⇒
	// 客户端重传分片，避免复用已被回收的分片文件）。
	if stale, serr := s.chunkRepo.GetByUploadIDUnscoped(ctx, req.UploadID); serr == nil && stale != nil {
		if !chunkSessionOwnedBy(stale, req.OwnerIP, req.UserID, req.SessionToken) &&
			s.sessionHasChunks(ctx, stale.UploadID) {
			return nil, ErrSessionConflict
		}
		if err := s.chunkRepo.ReviveSession(ctx, req.UploadID); err != nil {
			return nil, err
		}
		revived, rerr := s.chunkRepo.GetByUploadID(ctx, req.UploadID)
		if rerr != nil {
			return nil, rerr
		}
		// 归属可能随会话一起被软删重置 ⇒ 复活后按本次请求更新（沿用 live 分支口径）
		if req.OwnerIP != "" && revived.OwnerIP != req.OwnerIP {
			updates := map[string]interface{}{"owner_ip": req.OwnerIP}
			if req.UserID != nil {
				updates["user_id"] = *req.UserID
			}
			if err := s.chunkRepo.UpdateOwner(ctx, revived.UploadID, updates); err == nil {
				revived.OwnerIP = req.OwnerIP
				if req.UserID != nil {
					revived.UserID = req.UserID
				}
			}
		}
		return controlToResp(revived), nil
	}

	// 创建控制记录（chunk_index = -1）
	// ChunkHash 记录客户端提供的整文件哈希（秒传检索用；分片哈希存各分片记录）
	chunk := &model.UploadChunk{
		UploadID:    req.UploadID,
		ChunkIndex:  -1, // 控制记录标识
		TotalChunks: req.TotalChunks,
		FileSize:    req.FileSize,
		ChunkSize:   req.ChunkSize,
		FileName:    utils.SanitizeFileName(req.FileName),
		OwnerIP:     req.OwnerIP,
		UserID:      req.UserID,
		Status:      "pending",
	}

	err = s.chunkRepo.Create(ctx, chunk)
	if err != nil {
		return nil, err
	}

	return controlToResp(chunk), nil
}

// controlToResp 控制记录（chunk_index=-1）转响应
func controlToResp(c *model.UploadChunk) *ChunkResp {
	return &ChunkResp{
		ID:          c.ID,
		UploadID:    c.UploadID,
		ChunkIndex:  c.ChunkIndex,
		ChunkHash:   c.ChunkHash,
		TotalChunks: c.TotalChunks,
		FileSize:    c.FileSize,
		ChunkSize:   c.ChunkSize,
		FileName:    c.FileName,
		Completed:   c.Completed,
		Status:      c.Status,
	}
}

// UploadChunk 上传单个分片
func (s *Service) UploadChunk(ctx context.Context, req *UploadChunkReq) (*ChunkResp, error) {
	// 检查上传ID是否存在
	controlChunk, err := s.chunkRepo.GetByUploadID(ctx, req.UploadID)
	if err != nil {
		return nil, fmt.Errorf("upload ID not found: %v", err)
	}

	// 检查分片索引是否有效
	if req.ChunkIndex < 0 || req.ChunkIndex >= controlChunk.TotalChunks {
		return nil, errors.New("invalid chunk index")
	}

	// 检查分片是否已存在
	existingChunk, err := s.chunkRepo.GetChunkByIndex(ctx, req.UploadID, req.ChunkIndex)
	if err == nil && existingChunk.Completed {
		return &ChunkResp{
			ID:          existingChunk.ID,
			UploadID:    existingChunk.UploadID,
			ChunkIndex:  existingChunk.ChunkIndex,
			ChunkHash:   existingChunk.ChunkHash,
			TotalChunks: controlChunk.TotalChunks,
			FileSize:    controlChunk.FileSize,
			ChunkSize:   existingChunk.ChunkSize,
			FileName:    controlChunk.FileName,
			Completed:   existingChunk.Completed,
			Status:      existingChunk.Status,
		}, nil // 分片已完成，直接返回
	}

	// 创建或更新分片记录
	chunk := &model.UploadChunk{
		UploadID:   req.UploadID,
		ChunkIndex: req.ChunkIndex,
		ChunkHash:  req.ChunkHash,
		ChunkSize:  req.ChunkSize,
		Status:     "completed",
		Completed:  true,
	}

	// 幂等写入（2026-10-25 审计 A3）：**同一 (upload_id, chunk_index) 只有一行**。
	//
	// 修前：GetChunkByIndex 的 WHERE 含 `completed = true` ⇒ **未完成的旧行查不到**
	// ⇒ 分片失败后重传同索引会**再建一行**（下面的 Update 分支实为死代码）⇒
	// CountCompletedChunks 按**行数**计 ⇒ 「分片齐全」判定可虚高（声明 5 片
	// 只传 4 片也可能计成 5）⇒ 合并阶段才发现缺片。
	//
	// ⇒ 改用 dao.FirstOrCreateChunk（按 (upload_id, chunk_index) 命中即更新）：
	// 配合 (upload_id, chunk_index) 唯一索引（迁移 000006）⇒ DB 层也保证唯一。
	if err = s.chunkRepo.UpsertChunk(ctx, chunk); err != nil {
		return nil, err
	}

	return &ChunkResp{
		ID:          chunk.ID,
		UploadID:    chunk.UploadID,
		ChunkIndex:  chunk.ChunkIndex,
		ChunkHash:   chunk.ChunkHash,
		TotalChunks: controlChunk.TotalChunks,
		FileSize:    controlChunk.FileSize,
		ChunkSize:   chunk.ChunkSize,
		FileName:    controlChunk.FileName,
		Completed:   chunk.Completed,
		Status:      chunk.Status,
	}, nil
}

// CheckUploadProgress 检查上传进度
func (s *Service) CheckUploadProgress(ctx context.Context, uploadID string) (*ProgressResp, error) {
	controlChunk, err := s.chunkRepo.GetByUploadID(ctx, uploadID)
	if err != nil {
		return nil, err
	}

	completedChunks, err := s.chunkRepo.CountCompletedChunks(ctx, uploadID)
	if err != nil {
		return nil, err
	}

	var progress float64
	if controlChunk.TotalChunks > 0 {
		progress = float64(completedChunks) / float64(controlChunk.TotalChunks) * 100
	}

	return &ProgressResp{
		UploadID:        uploadID,
		TotalChunks:     controlChunk.TotalChunks,
		CompletedChunks: completedChunks,
		Progress:        progress,
		Status:          controlChunk.Status,
	}, nil
}

// CompleteUpload 完成上传
func (s *Service) CompleteUpload(ctx context.Context, uploadID string) error {
	controlChunk, err := s.chunkRepo.GetByUploadID(ctx, uploadID)
	if err != nil {
		return err
	}

	completedChunks, err := s.chunkRepo.CountCompletedChunks(ctx, uploadID)
	if err != nil {
		return err
	}

	if completedChunks < int64(controlChunk.TotalChunks) {
		return errors.New("not all chunks are completed")
	}

	// 更新控制记录状态为已完成
	return s.chunkRepo.UpdateChunkCompleted(ctx, uploadID, -1, "")
}

// GetUploadList 获取上传列表
func (s *Service) GetUploadList(ctx context.Context, page, pageSize int) ([]*ChunkResp, int64, error) {
	chunks, total, err := s.chunkRepo.GetUploadList(ctx, page, pageSize)
	if err != nil {
		return nil, 0, err
	}

	resps := make([]*ChunkResp, len(chunks))
	for i, chunk := range chunks {
		resps[i] = &ChunkResp{
			ID:          chunk.ID,
			UploadID:    chunk.UploadID,
			ChunkIndex:  chunk.ChunkIndex,
			ChunkHash:   chunk.ChunkHash,
			TotalChunks: chunk.TotalChunks,
			FileSize:    chunk.FileSize,
			ChunkSize:   chunk.ChunkSize,
			FileName:    chunk.FileName,
			Completed:   chunk.Completed,
			Status:      chunk.Status,
		}
	}

	return resps, total, nil
}

// DeleteUpload 删除上传
func (s *Service) DeleteUpload(ctx context.Context, uploadID string) error {
	return s.chunkRepo.DeleteByUploadID(ctx, uploadID)
}

// GetUploadedChunkIndexes 获取已上传分片的索引列表
func (s *Service) GetUploadedChunkIndexes(ctx context.Context, uploadID string) ([]int, error) {
	return s.chunkRepo.GetUploadedChunkIndexes(ctx, uploadID)
}

// sessionHasChunks 会话是否已含任何分片数据（空会话=可安全回收的孤儿，init 可认领）。
func (s *Service) sessionHasChunks(ctx context.Context, uploadID string) bool {
	indexes, err := s.GetUploadedChunkIndexes(ctx, uploadID)
	return err == nil && len(indexes) > 0
}

// GetUploadInfo 获取上传信息（包括文件名、大小等）
func (s *Service) GetUploadInfo(ctx context.Context, uploadID string) (*model.UploadChunk, error) {
	return s.chunkRepo.GetByUploadID(ctx, uploadID)
}

// FindQuickUploadSource 秒传源检索（2026-10-12，D1=A 同用户内去重）：
// 返回调用者自己历史同 hash+size 的最新可用分享；命中后由 handler 按**本次
// 元数据**建新分享引用其存储对象（与 presign 通道同语义，2026-10-12 起
// chunk init 契约携带 expire/password 元数据）。
//
// 此前"命中复用自己旧码"语义已废弃：旧分享的过期/密码/名称与本次请求意图
// 不符（静默丢弃本次元数据）；全库跨用户匹配更早废弃——存在性 oracle。
func (s *Service) FindQuickUploadSource(ctx context.Context, userID uint, fileHash string, fileSize int64) (*model.FileCode, error) {
	if userID == 0 || fileHash == "" || fileSize <= 0 {
		return nil, errors.New("invalid quick upload request")
	}
	fc, err := s.fileCodeRepo.FindQuickUploadSource(ctx, userID, fileHash, fileSize)
	if err != nil || fc == nil {
		return nil, errors.New("share source not found")
	}
	// 时间过期**或**次数耗尽都不可作秒传源：次数耗尽的分享其物理文件会被
	// 清理任务按 expired_count=0 删除 ⇒ 新分享指向一个即将消失的文件。
	if !fc.IsPickupAvailable() {
		return nil, errors.New("share expired")
	}
	return fc, nil
}
