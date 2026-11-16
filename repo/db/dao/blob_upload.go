// blob_upload.go —— 分片/presign 会话的账本接入（core#172 第三片·拆分项 3）：
// Pending→Uploading→Committed 生产流转的数据面。
//
// # 与 EnsureBlobWithRef（blob.go，首片）的分工
//
//	EnsureBlobWithRef —— 分享创建入口：建即 active + 挂引用（字节已提交 + 分享将发布）。
//
// 本文件          —— 上传会话入口：字节尚未提交/提交中，状态只能在
//
//	pending → uploading → committed 段内前进；分享创建时仍由
//	EnsureBlobWithRef 沿 CanReach 推进到 active（首片已具备）。
//
// # 两个入口
//
//	EnsureUploadBlob  —— 「落行或推进到 state」：行不存在则以 state 建行（分片合并
//	                     以 committed 入口=字节已落盘；presign Init 以 pending 入口=
//	                     会话已建、写未开始）；已存在则沿合法链前进。
//	AdvanceUploadBlob —— 「只推进不建行」：行不存在 = 该会话从未落账 ⇒ noop
//	                     （主链不受阻，分享创建时 EnsureBlobWithRef 兜底建行）。
//
// # 推进语义（负对照的真相源）
//
//   - 线性序 pending < uploading < committed < active：只能前进，**回退一律拒绝**
//     （如 active→committed、committed→uploading ⇒ ErrBlobNotReusable）；
//   - aborted 是旁支终态：仅 pending/uploading/committed 单跳可入（状态机边），
//     active（分享已发布）/终态一律拒绝；
//   - 目标态已在 ⇒ 幂等 noop（重复 Complete/重放 PUT 不重复计数）；
//   - 多步推进逐跳 CAS（WHERE state=from），并发下只有一方成功；竞争重读重试，
//     上限内未收敛报错（调用方 best-effort 告警，不阻断上传主链）。
//
// 推进结果（outcome）与 pkg/metrics/upload_state.go 的 outcome 标签同源；
// 指标在本层记录（推进的唯一收口，任何调用方都不会漏记）。
package dao

import (
	"context"
	"errors"
	"fmt"

	"github.com/pigeonbox/core/pkg/metrics"
	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// 上传段推进结果（调用方按此记 pb_blob_upload_transitions_total 的 outcome 标签）。
const (
	UploadTxCreated  = "created"  // 首次落行（from=none）
	UploadTxAdvanced = "advanced" // 沿合法链前进
	UploadTxNoop     = "noop"     // 已在目标态 / 行不存在
	UploadTxRejected = "rejected" // 非法推进被状态机拒绝
	UploadTxError    = "error"    // 数据库层错误
)

// uploadStages 上传段线性序（前进合法、后退拒绝；active 仅作序端点——
// 推进到 active 属分享创建入口 EnsureBlobWithRef，不经本文件）。
var uploadStages = [...]string{
	model.BlobPending, model.BlobUploading, model.BlobCommitted, model.BlobActive,
}

// uploadStageIndex 状态在线性序中的位置；不在序中返回 -1。
func uploadStageIndex(s string) int {
	for i, st := range uploadStages {
		if st == s {
			return i
		}
	}
	return -1
}

// validUploadTarget 合法推进目标：上传段三态 + 旁支终态 aborted。
// （active 不在列：挂引用推进到 active 是 EnsureBlobWithRef 的职责。）
func validUploadTarget(state string) bool {
	switch state {
	case model.BlobPending, model.BlobUploading, model.BlobCommitted, model.BlobAborted:
		return true
	}
	return false
}

// nextUploadHop 给定现态与目标态，返回**一步合法跳**；不可达返回 false。
//
//	target=aborted：现态必须有 →aborted 的状态机边（单跳即达）；
//	其余目标：线性序内前进一格（表内边与序一致，CanBlobTransition 复核）。
func nextUploadHop(cur, target string) (string, bool) {
	if target == model.BlobAborted {
		if cur == model.BlobAborted {
			return "", false // 已在目标态（调用方先判 noop，这里双保险）
		}
		if model.CanBlobTransition(cur, model.BlobAborted) {
			return model.BlobAborted, true
		}
		return "", false
	}
	iCur, iTo := uploadStageIndex(cur), uploadStageIndex(target)
	if iCur < 0 || iTo < 0 || iCur >= iTo {
		return "", false // 未知态 / 不在序中（trashed/purge/purged）/ 回退
	}
	next := uploadStages[iCur+1]
	if !model.CanBlobTransition(cur, next) {
		return "", false // 状态机表与序不一致时以表为准（防御）
	}
	return next, true
}

// EnsureUploadBlob 上传会话账本落行/推进（一个事务）：
//
//   - 行不存在：以 state 建行（size/fileHash 一并落；state 须为上传段入口态
//     pending/uploading/committed——aborted 不是建行目标）⇒ created（from=none）；
//   - 行已存在：先回填 size/fileHash（只填空，不覆盖已知值），再按推进语义走：
//     已在目标态 ⇒ noop；可沿合法链前进 ⇒ 逐跳 CAS ⇒ advanced；
//     回退/终态/未知态 ⇒ ErrBlobNotReusable。
//
// 返回 (from, outcome, err)：from=推进前现态（无行=none）；outcome 见 UploadTx*。
// 每次调用记一笔 pb_blob_upload_transitions_total（本层是推进的唯一收口，
// 指标在此记录不漏任何调用方）。
func (r *BlobRepository) EnsureUploadBlob(ctx context.Context, objectKey string, size int64, fileHash string, state string) (string, string, error) {
	from, outcome, err := r.ensureUploadBlob(ctx, objectKey, size, fileHash, state)
	metrics.RecordBlobUploadTransition(from, state, outcome)
	return from, outcome, err
}

func (r *BlobRepository) ensureUploadBlob(ctx context.Context, objectKey string, size int64, fileHash string, state string) (string, string, error) {
	if objectKey == "" {
		return "none", UploadTxError, errors.New("EnsureUploadBlob: objectKey 必填")
	}
	if state == model.BlobAborted {
		return "none", UploadTxError, errors.New("EnsureUploadBlob: aborted 不是建行目标")
	}
	if uploadStageIndex(state) < 0 || state == model.BlobActive {
		return "none", UploadTxError, fmt.Errorf("EnsureUploadBlob: 非法建行目标 %q", state)
	}
	from, outcome := "none", UploadTxCreated
	err := r.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var blob model.Blob
		err := tx.Where("object_key = ?", objectKey).First(&blob).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			created := model.Blob{
				ObjectKey: objectKey,
				Size:      size,
				FileHash:  fileHash,
				State:     state,
			}
			if cerr := tx.Create(&created).Error; cerr != nil {
				return cerr // 含并发唯一索引冲突
			}
			return nil
		}
		if err != nil {
			return err
		}
		from = blob.State
		outcome = UploadTxNoop
		fillUploadBlobTx(tx, &blob, size, fileHash)
		return advanceUploadBlobTx(tx, &blob, state, &outcome)
	})
	if err != nil {
		if isUploadTxDone(outcome, err) {
			return from, outcome, err // rejected：状态机拒绝（带语义的错误）
		}
		return from, UploadTxError, err
	}
	return from, outcome, nil
}

// AdvanceUploadBlob 只推进已存在的行（不建行）：行不存在 ⇒ (none, noop, nil)。
// 语义与 EnsureUploadBlob 的推进段一致（线性前进/aborted 单跳/幂等 noop/回退拒绝）。
// 同样记 pb_blob_upload_transitions_total（见 EnsureUploadBlob 注释）。
func (r *BlobRepository) AdvanceUploadBlob(ctx context.Context, objectKey string, state string) (string, string, error) {
	from, outcome, err := r.advanceUploadBlob(ctx, objectKey, state)
	metrics.RecordBlobUploadTransition(from, state, outcome)
	return from, outcome, err
}

func (r *BlobRepository) advanceUploadBlob(ctx context.Context, objectKey string, state string) (string, string, error) {
	if objectKey == "" {
		return "none", UploadTxError, errors.New("AdvanceUploadBlob: objectKey 必填")
	}
	if !validUploadTarget(state) {
		return "none", UploadTxError, fmt.Errorf("AdvanceUploadBlob: 非法目标 %q", state)
	}
	from, outcome := "none", UploadTxNoop
	err := r.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var blob model.Blob
		err := tx.Where("object_key = ?", objectKey).First(&blob).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil // 未落账：noop（调用方按 best-effort 处理）
		}
		if err != nil {
			return err
		}
		from = blob.State
		if blob.State == state {
			outcome = UploadTxNoop
			return nil
		}
		outcome = UploadTxAdvanced
		return advanceUploadBlobTx(tx, &blob, state, &outcome)
	})
	if err != nil {
		if isUploadTxDone(outcome, err) {
			return from, outcome, err
		}
		return from, UploadTxError, err
	}
	return from, outcome, nil
}

// fillUploadBlobFields 只填空回填：size>0 且行内为 0、fileHash 非空且行内为空
// 才写（不覆盖已知值——申报大小/哈希以先到为准，避免并发会话互相改写）。
func fillUploadBlobTx(tx *gorm.DB, blob *model.Blob, size int64, fileHash string) {
	updates := map[string]any{}
	if size > 0 && blob.Size == 0 {
		updates["size"] = size
	}
	if fileHash != "" && blob.FileHash == "" {
		updates["file_hash"] = fileHash
	}
	if len(updates) > 0 {
		_ = tx.Model(&model.Blob{}).Where("id = ?", blob.ID).Updates(updates).Error
		// 回填失败不改变推进语义（best-effort）：忽略错误，主推进照走。
	}
}

// advanceUploadBlobTx 把行推进到 target（事务内，逐跳 CAS），按结果改写 *outcome。
// 拒绝时返回 ErrBlobNotReusable（*outcome=UploadTxRejected）。
func advanceUploadBlobTx(tx *gorm.DB, blob *model.Blob, target string, outcome *string) error {
	cur := blob.State
	if cur == target {
		*outcome = UploadTxNoop
		return nil
	}
	// 竞争上限：每轮 CAS 未命中则重读重试（会话完成面有跨副本锁，实际极少触发）。
	for attempt := 0; attempt < 4; attempt++ {
		next, ok := nextUploadHop(cur, target)
		if !ok {
			*outcome = UploadTxRejected
			return ErrBlobNotReusable
		}
		res := tx.Model(&model.Blob{}).
			Where("id = ? AND state = ?", blob.ID, cur).
			Update("state", next)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected > 0 {
			cur = next
			if cur == target {
				blob.State = cur
				*outcome = UploadTxAdvanced
				return nil
			}
			continue
		}
		// CAS 未命中：并发方已改状态，重读再判。
		var latest model.Blob
		if err := tx.First(&latest, blob.ID).Error; err != nil {
			return err
		}
		cur = latest.State
	}
	return fmt.Errorf("AdvanceUploadBlob: 推进竞争超限（%s → %s）", cur, target)
}

// isUploadTxDone 判定 err 是否属于「已带语义 outcome 的错误」（rejected），
// 用于把状态机拒绝与数据库错误分开归 outcome。
func isUploadTxDone(outcome string, err error) bool {
	return outcome == UploadTxRejected && errors.Is(err, ErrBlobNotReusable)
}
