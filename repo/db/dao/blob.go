// blob.go 文件生命周期统一模型的数据面（core#172 首片）：
// Blob/Ensure 双写、引用释放（硬删才释放）、回收候选列举、CAS 状态转移。
//
// 与既有数据面的关系（收编，不另起炉灶）：
//   - file_codes/file_code_files 仍是分享与文件清单的**唯一真相源**——本文件
//     不改它们一行；blobs/file_references 是并行账本（首片不做存量回填，
//     回填/懒迁移口径见 issue 拆分清单）。
//   - 物理删除前的路径引用兜底仍走 FileCodeRepository.CountFilePathReferences
//     （含软删行、含无账本行的存量分享）——账本归零但路径仍被引用时**不删**，
//     由 janitor 转回 active（见 app/admin/blob_gc.go）。
package dao

import (
	"context"
	"errors"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// ErrBlobNotReusable 目标 blob 行存在但其状态无法沿合法链推进到 active
// （purge/purged/aborted 终态或回收进行中）——调用方只告警不阻断分享创建
// （账本缺行是泄漏方向，既有 janitor 对账兜底；误删才是事故方向）。
var ErrBlobNotReusable = errors.New("blob 状态无法推进到 active")

// BlobRepository blobs / file_references 账本 DAO（构造注入，同 doc.go 约定）。
type BlobRepository struct {
	conn *gorm.DB
}

// NewBlobRepository 构造 repository；gormDB 必填。
func NewBlobRepository(gormDB *gorm.DB) *BlobRepository {
	return &BlobRepository{conn: gormDB}
}

func (r *BlobRepository) db() *gorm.DB { return r.conn }

// EnsureBlobWithRef 分享创建的账本双写（一个事务）：
//
//  1. blobs 按 object_key 取或建——不存在则**建即 active**（写路径进入点是
//     「对象字节已提交 + 分享即将发布」；Pending→Uploading→Committed 段留给
//     分片/presign 路径的 Reference 化，见 issue 拆分清单）；已存在则用
//     model.CanReach 校验「能沿合法链走到 active」——能则 CAS 推进（如
//     trashed 宽限期内被重新引用 ⇒ 恢复），不能（purge/purged/aborted）⇒
//     ErrBlobNotReusable。
//  2. file_references 按 (blob_id, file_code_id) 取或建（幂等：重试/重复调用
//     不产生第二行；已 released 的引用不复活——硬删不可逆）。
//
// 全程一个事务：blob 建了但 ref 失败 ⇒ 回滚不留半截账。
func (r *BlobRepository) EnsureBlobWithRef(ctx context.Context, objectKey string, size int64, fileHash string, fileCodeID uint) (uint, error) {
	if objectKey == "" || fileCodeID == 0 {
		return 0, errors.New("EnsureBlobWithRef: objectKey/fileCodeID 必填")
	}
	var blobID uint
	err := r.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var blob model.Blob
		err := tx.Where("object_key = ?", objectKey).First(&blob).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			// 新行：建即 active（见方法注释的取舍）。
			blob = model.Blob{
				ObjectKey: objectKey,
				Size:      size,
				FileHash:  fileHash,
				State:     model.BlobActive,
			}
			if cerr := tx.Create(&blob).Error; cerr != nil {
				return cerr // 含并发唯一索引冲突；调用方按幂等重试
			}
		case err != nil:
			return err
		case blob.State == model.BlobActive:
			// 已是存活态：只补引用。
		default:
			// 存量行推进到 active：必须存在合法链（状态机守卫）。
			if !model.CanReach(blob.State, model.BlobActive) {
				return ErrBlobNotReusable
			}
			// CAS 推进（WHERE state=old）：并发下只有一方成功；0 行 = 对方
			// 已推进，重读确认即可。
			res := tx.Model(&model.Blob{}).
				Where("id = ? AND state = ?", blob.ID, blob.State).
				Update("state", model.BlobActive)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				var cur model.Blob
				if err := tx.First(&cur, blob.ID).Error; err != nil {
					return err
				}
				if cur.State != model.BlobActive {
					return ErrBlobNotReusable
				}
			}
		}
		blobID = blob.ID

		// 引用行：取或建（幂等）。
		var ref model.FileReference
		err = tx.Where("blob_id = ? AND file_code_id = ?", blobID, fileCodeID).First(&ref).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			ref = model.FileReference{
				BlobID:     blobID,
				FileCodeID: fileCodeID,
				State:      model.RefActive,
			}
			if cerr := tx.Create(&ref).Error; cerr != nil {
				return cerr
			}
		case err != nil:
			return err
		case ref.State == model.RefActive:
			// 幂等：引用已在。
		default:
			// released 引用不复活（硬删不可逆）——同一分享 id 不可能再建分享，
			// 出现即数据异常，显式报错比静默改账诚实。
			return errors.New("EnsureBlobWithRef: 引用已 released，不复活")
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return blobID, nil
}

// ReleaseByShare 分享**硬删**后的引用释放（一个事务）：
//
//  1. 该分享全部 active 引用 → released（记 released_at）；
//  2. 逐个受影响 blob 复核存活引用数，归零 ⇒ active→trashed（记 trashed_at
//     = 回收宽限期起点）；未归零 ⇒ 保持 active（负对照：共享对象不进回收）。
//
// 软删**不调用**本方法（软删不释放——回收站行仍占对象，恢复后对象必须在）。
// 幂等：无 active 引用时返回 0，不报错。
// 返回释放的引用行数。
func (r *BlobRepository) ReleaseByShare(ctx context.Context, fileCodeID uint) (int64, error) {
	if fileCodeID == 0 {
		return 0, errors.New("ReleaseByShare: fileCodeID 必填")
	}
	var released int64
	err := r.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var refs []model.FileReference
		if err := tx.Where("file_code_id = ? AND state = ?", fileCodeID, model.RefActive).
			Find(&refs).Error; err != nil {
			return err
		}
		if len(refs) == 0 {
			return nil // 幂等
		}
		now := time.Now()
		res := tx.Model(&model.FileReference{}).
			Where("file_code_id = ? AND state = ?", fileCodeID, model.RefActive).
			Updates(map[string]any{"state": model.RefReleased, "released_at": now})
		if res.Error != nil {
			return res.Error
		}
		released = res.RowsAffected

		// 逐 blob 复核归零（共享对象：还有别的引用 ⇒ 不进回收）。
		seen := make(map[uint]bool, len(refs))
		for _, ref := range refs {
			if seen[ref.BlobID] {
				continue
			}
			seen[ref.BlobID] = true
			var alive int64
			if err := tx.Model(&model.FileReference{}).
				Where("blob_id = ? AND state = ?", ref.BlobID, model.RefActive).
				Count(&alive).Error; err != nil {
				return err
			}
			if alive > 0 {
				continue // 负对照①：引用未归零不进回收
			}
			// CAS：只在仍为 active 时转 trashed（并发释放只有一方成功）。
			if err := tx.Model(&model.Blob{}).
				Where("id = ? AND state = ?", ref.BlobID, model.BlobActive).
				Updates(map[string]any{"state": model.BlobTrashed, "trashed_at": now}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return released, nil
}

// ListReclaimable 回收候选：
//
//   - state=trashed 且 trashed_at 早于 cutoff（过宽限）；
//   - state=purge（上一轮删对象失败的重试；进 purge 必已过宽限）。
//
// 不含 active（有引用或账本未归零）与终态。
func (r *BlobRepository) ListReclaimable(ctx context.Context, cutoff time.Time) ([]model.Blob, error) {
	var rows []model.Blob
	err := r.db().WithContext(ctx).
		Where("(state = ? AND trashed_at IS NOT NULL AND trashed_at < ?) OR state = ?",
			model.BlobTrashed, cutoff, model.BlobPurge).
		Order("id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// CountActiveRefs 某 blob 的存活引用数（账本口径）。
func (r *BlobRepository) CountActiveRefs(ctx context.Context, blobID uint) (int64, error) {
	var n int64
	err := r.db().WithContext(ctx).Model(&model.FileReference{}).
		Where("blob_id = ? AND state = ?", blobID, model.RefActive).
		Count(&n).Error
	return n, err
}

// TransitionBlob CAS 状态转移：model.CanBlobTransition 合法性（Go 层状态机守卫）
// + UPDATE ... WHERE state=from（DB 层并发守卫）。返回是否实际发生转移
// （false = 并发下状态已变或 from 不符，调用方按跳过处理）。
//
// 时间戳随目标态落：→trashed 记 trashed_at，→purged 记 purged_at。
func (r *BlobRepository) TransitionBlob(ctx context.Context, blobID uint, from, to string) (bool, error) {
	if !model.CanBlobTransition(from, to) {
		return false, errors.New("TransitionBlob: 非法转移 " + from + " → " + to)
	}
	updates := map[string]any{"state": to}
	switch to {
	case model.BlobTrashed:
		updates["trashed_at"] = time.Now()
	case model.BlobPurged:
		updates["purged_at"] = time.Now()
	}
	res := r.db().WithContext(ctx).Model(&model.Blob{}).
		Where("id = ? AND state = ?", blobID, from).
		Updates(updates)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}
