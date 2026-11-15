package model

import "time"

// 文件生命周期统一模型（core#172 首片）：Blob（物理对象）与 FileReference（分享引用）
// 职责分离 + 显式状态机。
//
// # 为什么要拆开
//
// 此前「一个文件对象有没有别的分享在用」是**隐式**的：每次删除时按存储路径现查
// file_codes/file_code_files 行数（CountFilePathReferences，含软删行）。口径散在
// 删除路径各处，状态（在传/已提交/待回收/已回收）没有地方表达，回收时机只能
// 「立即删 or 等 janitor 扫盘」两极。
//
// ⇒ 本文件把两件事各自落成一张表：
//
//	blobs           —— 物理对象一行（object_key = 存储相对路径，唯一），生命周期走显式状态机；
//	file_references —— 「哪个分享引用哪个对象」一行，硬删才释放（软删保留，回收站行仍占对象）。
//
// # 状态机（issue #172 给定链）
//
//	Pending → Uploading → Committed → Active → Trashed → Purge → Purged / Aborted
//
// 首片生产流转只走到 Active ↔ Trashed → Purge → Purged（写路径建即 Active、
// 硬删归零转 Trashed、janitor 过宽限转 Purge 删对象转 Purged）；
// Pending/Uploading/Committed 供分片/presign 路径 Reference 化（拆分项）接入后启用，
// 本文件先把转移合法性钉死（纯函数 + 表驱动测试，非法转移一律拒绝）。
//
// # 不变量
//
//   - 引用计数回收：file_references 中 state=active 的行数 = 该对象的存活引用数；
//     归零且过宽限才允许回收（janitor/blob_gc.go）。
//   - 软删不释放引用：删除分享的软删路径不碰 file_references —— 回收站里的分享
//     恢复后对象必须还在（与 CountFilePathReferences 含软删行的既有口径一致）。
//     只有**硬删**（彻底删除）才释放引用。

// Blob 生命周期状态（issue #172 状态机）。
const (
	// BlobPending 对象已定位、写入尚未开始（分片 init / presign 建会话）。
	BlobPending = "pending"
	// BlobUploading 对象写入进行中（分片上传 / 直传 PUT）。
	BlobUploading = "uploading"
	// BlobCommitted 对象字节写入完成，尚无分享引用（上传完成、建分享之前）。
	BlobCommitted = "committed"
	// BlobActive 有 ≥1 条存活引用（生产写路径的建即态：分享创建时对象已提交）。
	BlobActive = "active"
	// BlobTrashed 引用归零，进入回收宽限期（trashed_at = 宽限期起点）。
	BlobTrashed = "trashed"
	// BlobPurge 宽限期已过，回收进行中（删物理对象 → Purged）。
	BlobPurge = "purge"
	// BlobPurged 物理对象已删除（终态）。
	BlobPurged = "purged"
	// BlobAborted 上传中止/回收失败中止（终态；对象字节由 janitor 兜底清理）。
	BlobAborted = "aborted"
)

// blobTransitions 合法转移表（唯一真相源；非法转移由 CanBlobTransition 拒绝）。
//
// 设计取舍：
//   - trashed → active：宽限期内对象仍可能被重新引用（秒传/自定义场景），恢复而非拒绝；
//   - purge → purged / aborted：issue 正文「Purge → Purged/Aborted」——删除成功或中止；
//   - purged / aborted 为终态，不许复活（对象已删/已判废，复活会指向不存在的字节）；
//   - pending/uploading/committed → aborted：上传失败链。
var blobTransitions = map[string][]string{
	BlobPending:   {BlobUploading, BlobAborted},
	BlobUploading: {BlobCommitted, BlobAborted},
	BlobCommitted: {BlobActive, BlobAborted},
	BlobActive:    {BlobTrashed},
	BlobTrashed:   {BlobActive, BlobPurge},
	BlobPurge:     {BlobPurged, BlobAborted},
	BlobPurged:    {},
	BlobAborted:   {},
}

// CanBlobTransition 判定 Blob 状态转移是否合法（纯函数）。
//
// 未知状态（含空串）一律 false——状态机不允许「从无到有」造出新状态。
func CanBlobTransition(from, to string) bool {
	for _, next := range blobTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// CanReach 判定能否沿合法转移链从 from 走到 to（含 from==to，0 步即达）。
//
// 用途：写路径要把既有行「推进到目标态」（如 EnsureBlob 把 pending/uploading/
// committed/trashed 行推进到 active）——目标可能不在 from 的**直接**出边里
// （如 pending → active 要走三步），但必须存在一条全合法的链；不存在 ⇒ 拒绝。
// 与 CanBlobTransition 的区别：那个问「一步是否合法」，这个问「可达性」。
func CanReach(from, to string) bool {
	if from == to {
		return ValidBlobState(from)
	}
	if !ValidBlobState(from) || !ValidBlobState(to) {
		return false
	}
	seen := map[string]bool{from: true}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range blobTransitions[cur] {
			if next == to {
				return true
			}
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

// BlobValidStates 全部合法状态（测试/校验用）。
func BlobValidStates() []string {
	return []string{
		BlobPending, BlobUploading, BlobCommitted, BlobActive,
		BlobTrashed, BlobPurge, BlobPurged, BlobAborted,
	}
}

// ValidBlobState 状态取值白名单（写入口统一校验，与 ValidShareStatus 同风格）。
func ValidBlobState(s string) bool {
	for _, st := range BlobValidStates() {
		if st == s {
			return true
		}
	}
	return false
}

// FileReference 状态：分享对对象的引用。
const (
	// RefActive 存活引用（含其分享被软删进回收站的情形——恢复后对象必须还在）。
	RefActive = "active"
	// RefReleased 已释放（分享被**硬删**；仅此路径释放，见文件头不变量）。
	RefReleased = "released"
)

// refTransitions 引用状态转移表：只释放、不复活（硬删不可逆）。
var refTransitions = map[string][]string{
	RefActive:   {RefReleased},
	RefReleased: {},
}

// CanRefTransition 判定 FileReference 状态转移是否合法（纯函数）。
func CanRefTransition(from, to string) bool {
	for _, next := range refTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// Blob 物理对象（一行 = 一个存储相对路径的对象）。
//
// 不内嵌 gorm.Model：软删除对本模型无意义（生命周期由 State 表达，
// Purged 后行保留作账本），gorm.DeletedAt 会与状态机抢语义。
type Blob struct {
	ID uint `gorm:"primaryKey" json:"id"`
	// ObjectKey 存储相对路径（唯一索引）：与 file_codes.file_path 同值域。
	ObjectKey string `gorm:"uniqueIndex;size:255" json:"object_key"`
	Size      int64  `gorm:"default:0" json:"size"`
	// FileHash 内容哈希（秒传去重键；可空——存量/文本对象无哈希）。
	FileHash string `gorm:"size:64;index" json:"file_hash"`
	// State 生命周期状态（见 Blob* 常量；default active = 写路径建即态）。
	State string `gorm:"size:20;default:'active';index" json:"state"`
	// TrashedAt 进入宽限期的时间（State=trashed 时非空；宽限判据）。
	TrashedAt *time.Time `json:"trashed_at"`
	// PurgedAt 物理对象删除完成时间（State=purged 时非空）。
	PurgedAt *time.Time `json:"purged_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 显式表名（注册表/换库搬运按此对账）。
func (Blob) TableName() string { return "blobs" }

// FileReference 分享 → 物理对象的引用（一个分享可引用多个对象：多文件）。
//
// 软删不释放（见文件头不变量）；硬删释放 = State 置 released（行保留作账本）。
type FileReference struct {
	ID uint `gorm:"primaryKey" json:"id"`
	// BlobID 被引用的对象。
	BlobID uint `gorm:"index;uniqueIndex:idx_file_ref_blob_share" json:"blob_id"`
	// FileCodeID 引用方分享（file_codes.id）。
	FileCodeID uint `gorm:"index;uniqueIndex:idx_file_ref_blob_share" json:"file_code_id"`
	// State RefActive / RefReleased。
	State string `gorm:"size:20;default:'active'" json:"state"`
	// ReleasedAt 硬删释放时间（State=released 时非空）。
	ReleasedAt *time.Time `json:"released_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 显式表名。
func (FileReference) TableName() string { return "file_references" }
