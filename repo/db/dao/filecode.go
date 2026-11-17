package dao

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pigeonbox/core/internal/dblock"
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

// Create 落库分享记录。
//
// 不变量（core#220）：ExpiredCount ∈ {-1} ∪ (0,+∞)。-1=无限，>0=剩余；
// 0=已耗尽（不是可创建的初值，count+0 会建出「建完即死」的链接），
// <-1 是历史脏值（IsCountExhausted 只判 ==0 ⇒ 负值会被当成「还有次数」）。
// 入口已用 utils.CheckCountExpireValue 显式拒绝，这里再兜一层，保证任何
// 新调用方都写不出越界行。
func (r *FileCodeRepository) Create(ctx context.Context, fileCode *model.FileCode) error {
	if fileCode != nil && fileCode.ExpiredCount < -1 {
		return fmt.Errorf("非法的 expired_count=%d（合法值：-1=无限，>0=剩余）", fileCode.ExpiredCount)
	}
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

// FirstByUploadIDOwnedBy 按上传会话 ID 回读**属于该调用方**的已建分享
// （上传完成面的幂等回读，2026-11-06 P0 修复）。
//
// 背景（P0，一句话）：upload_id 是**客户端自报的可推导值**——分片通道
// `uploadID := req.FileHash`（gen/handler/chunk），直传通道同理 ⇒ **任何拿着
// 同一份文件的人都能算出他人的 upload_id**。而 init 对「已完成会话」的复活
// 路径会把 owner_ip / user_id 改写成**新调用方**（这是 2026-10-30 修复
// 「同一文件 7 天内无法重传」的刻意设计，见 app/chunk/session_completed_conflict_test.go）
// ⇒ 随后的归属校验 ownedByCaller 会**通过** ⇒ 若幂等回读仍用无限定的
// FirstByUploadID ⇒ **把受害者的分享码交给攻击者**（连带其取件额度与备注）。
//
// ⇒ 回读必须自带属主判据：
//   - 登录身份：`user_id = ?`；
//   - 匿名身份：`user_id IS NULL AND owner_ip = ?`；空 IP 不参与匹配——
//     否则两个都取不到 IP 的调用方会互相命中（把修复重新打穿）。
//
// 属主不符 ⇒ 返回 gorm.ErrRecordNotFound（视为「没建过」）⇒ 调用方按首次
// 流程建**自己**的分享。代价：匿名用户**跨网络**重试会建出第二条分享（旧行为
// 是复用它人的）。这是安全与幂等之间必须换的一侧：宁可多一条自己的行，
// 不可把别人的行交出去。
func (r *FileCodeRepository) FirstByUploadIDOwnedBy(ctx context.Context, uploadID string, userID *uint, ownerIP string) (*model.FileCode, error) {
	if uploadID == "" {
		return nil, gorm.ErrRecordNotFound
	}
	q := r.db().WithContext(ctx).Where("upload_id = ? AND deleted_at IS NULL", uploadID)
	if userID != nil {
		q = q.Where("user_id = ?", *userID)
	} else {
		if ownerIP == "" {
			return nil, gorm.ErrRecordNotFound
		}
		q = q.Where("user_id IS NULL AND owner_ip = ?", ownerIP)
	}
	var fileCode model.FileCode
	if err := q.Order("id ASC").First(&fileCode).Error; err != nil {
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
// 路径比对在 SQL 侧按 file_path 精确比对：2026-10-26 起 file_codes 的 file_path
// 口径已统一为「含唯一文件名的完整相对路径」，不再有「目录+uuid 文件名」的第二种
// 落库布局，无需宽取候选再解析归一。
func (r *FileCodeRepository) CountFilePathReferences(ctx context.Context, filePath string, excludeFileCodeID uint) (int64, error) {
	if filePath == "" {
		return 0, nil
	}
	var rows []model.FileCode
	if err := r.db().WithContext(ctx).Unscoped().
		Where("file_path = ? AND id <> ?", filePath, excludeFileCodeID).
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

// Delete 软删单条分享（连带软删子行「同生共死」，与 DeleteByIDTx 同口径）。
func (r *FileCodeRepository) Delete(ctx context.Context, id uint) error {
	return r.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := softDeleteChildRows(tx, []uint{id}); err != nil {
			return err
		}
		return tx.Delete(&model.FileCode{}, id).Error
	})
}

// DeleteByFileCode 软删单条分享（连带软删子行「同生共死」）。
func (r *FileCodeRepository) DeleteByFileCode(ctx context.Context, fileCode *model.FileCode) error {
	return r.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := softDeleteChildRows(tx, []uint{fileCode.ID}); err != nil {
			return err
		}
		return tx.Delete(fileCode).Error
	})
}

func (r *FileCodeRepository) Count(ctx context.Context) (int64, error) {
	var count int64
	err := r.db().WithContext(ctx).Model(&model.FileCode{}).Count(&count).Error
	return count, err
}

// CountToday 已删除：全库无调用方（统计走 CountTodayUploads/CountCreatedBetween）。

// CountActive 已删除（2026-10-26 审计 P2）：全仓**无生产调用方**，且口径是错的
// ——原实现 `expired_at IS NULL OR expired_at > ? OR expired_count > 0` 把两个
// 独立闸（时间过期 / 次数耗尽）OR 在一起 ⇒ 「时间已过期但次数没用尽」的 count
// 样式分享被算成**活跃**，与 model.FileCode.IsExpired()（只判时间）结论相反
// ⇒ 一旦有人接上它，管理台统计虚高。
// ⇒ 删除（参照同文件 CountToday 的同规格删除）；确有需要时按 IsExpired /
// IsCountExhausted 的口径重建，并在注释里写清「活跃」的定义。
// 守卫见 filecode_active_test.go（方法不得复活 + 不得有调用方）。

func (r *FileCodeRepository) GetTotalSize(ctx context.Context) (int64, error) {
	var totalSize int64
	err := r.db().WithContext(ctx).Model(&model.FileCode{}).Select("COALESCE(SUM(size), 0)").Scan(&totalSize).Error
	return totalSize, err
}

// ── 配额闸（core#169）─────────────────────────────────────────────────────

// quotaGateWait 配额闸取锁等待上限（本仓多把闸共用一个量级；持有者是毫秒级
// 的「合计+判定+插入」，2 分钟上限只是防僵死的兜底，正常永远到不了）。
const quotaGateWait = 2 * time.Minute

// UnderQuotaGateTx 在**配额闸**串行段内执行 mutate（core#169）。
//
// 缺陷背景：checkUploadCaps / CheckQuota 是「读合计 → 比上限 → 放行」，插入
// 发生在之后（CreateShare/chunk/presign 完成链）——check-then-act 无原子性，
// N 个并发上传各自读到同一 used、全部通过判定后各自 insert ⇒ 实际占用可达
// quota + (N-1)×size。这是「绕过上传配额 + 打满磁盘」的直接通道。
//
// 修法（issue 方案「收进同一事务并串行化」）：mutate 在拿到**跨副本命名互斥**
// （db.QuotaGateKey，MySQL GET_LOCK / PG advisory lock / sqlite 进程条纹）之后
// 的单事务里执行——后到者读到的合计必然包含先到者的插入。站点配额与用户配额
// 的权威复核都应在 mutate 内做（见 app/share 的 quotaGateGuard；dao 不持配额
// 上限语义，只提供串行段）。
//
// 方言纪律：sqlite 层是进程内条纹互斥（standalone 单进程部署形态下与 DB 级
// 互斥等价）；MySQL/PG 是真会话锁，多副本部署下同样串行。持锁期间不要做慢
// IO——mutate 只做「合计 + 判定 + 写行」。
func (r *FileCodeRepository) UnderQuotaGateTx(ctx context.Context, mutate func(tx *gorm.DB) error) error {
	if mutate == nil {
		return errors.New("quota gate: nil mutate")
	}
	lock := dblock.NewDLock(r.conn)
	release, err := lock.Lock(ctx, dblock.QuotaGateKey())
	if err != nil {
		return fmt.Errorf("配额闸互斥获取失败: %w", err)
	}
	defer release()
	return r.conn.WithContext(ctx).Transaction(mutate)
}

// SumShareSizeIncludingDeletedTx 闸内**站点口径**合计（含回收站软删行、未去重
// ——与 GetLogicalShareSizeIncludingDeleted 同口径，tx 作用域保证读到本事务
// 可见的最新写入）。配额复核用；裸调用（闸外）等于回到 check-then-act。
func SumShareSizeIncludingDeletedTx(tx *gorm.DB) (int64, error) {
	var total int64
	err := tx.Unscoped().Model(&model.FileCode{}).
		Select("COALESCE(SUM(size), 0)").Scan(&total).Error
	return total, err
}

// SumShareSizeByUserTx 闸内**用户口径**合计（仅存活行——用户配额是逻辑口径，
// 与 GetTotalSizeByUserID 同域）。
func SumShareSizeByUserTx(tx *gorm.DB, userID uint) (int64, error) {
	var total int64
	err := tx.Model(&model.FileCode{}).
		Where("user_id = ?", userID).
		Select("COALESCE(SUM(size), 0)").Scan(&total).Error
	return total, err
}

// GetLogicalShareSizeIncludingDeleted 逻辑分享容量合计（core#193 正名，原名
// GetTotalSizeIncludingDeleted 的「物理口径」名不副实）：SUM(file_codes.size)。
// 含回收站软删行——purge 前分享行仍在，逻辑用量不回退（评审 D01 的本意）；
// **未按物理对象去重**：秒传/多分享复用同一 blob 时每个分享各计一次。
// ⇒ 这是「保守口径」：站点配额闸用它宁可高估也不低估；真实磁盘占用看
// GetPhysicalObjectUsage。用户配额（GetTotalSizeByUserID）是存活口径，不走此方法。
func (r *FileCodeRepository) GetLogicalShareSizeIncludingDeleted(ctx context.Context) (int64, error) {
	var totalSize int64
	err := r.db().WithContext(ctx).Unscoped().Model(&model.FileCode{}).
		Select("COALESCE(SUM(size), 0)").Scan(&totalSize).Error
	return totalSize, err
}

// GetPhysicalObjectUsage 物理对象占用（core#193 补口径）：按存储相对路径
// **去重**后的字节合计。数据面=file_codes 主表 ∪ file_code_files 子表的
// 非空 file_path（主表记首文件、子表记全部文件；秒传共享的同一物理路径
// 无论出现在哪张表都只计一次）。同路径出现尺寸漂移（脏数据）时取 MAX 的
// 确定值，不放大。含回收站软删行——purge 前对象仍占盘。
// 口径外：预览缩略图（janitor 对账管）、分片临时区（会话清理管）、
// uploads/ 之外的存储内部结构。
//
// SQL 三方言（sqlite/mysql/pg）通用：纯派生表 + UNION ALL + GROUP BY。
func (r *FileCodeRepository) GetPhysicalObjectUsage(ctx context.Context) (int64, error) {
	var usage int64
	err := r.db().WithContext(ctx).Raw(`
		SELECT COALESCE(SUM(sz), 0) FROM (
			SELECT file_path, MAX(sz) AS sz FROM (
				SELECT file_path, MAX(size) AS sz FROM file_codes
					WHERE file_path IS NOT NULL AND file_path <> '' GROUP BY file_path
				UNION ALL
				SELECT file_path, MAX(size) AS sz FROM file_code_files
					WHERE file_path IS NOT NULL AND file_path <> '' GROUP BY file_path
			) u GROUP BY file_path
		) t`).Scan(&usage).Error
	return usage, err
}

// GetDirectoryUsage 目录配额口径的**存活**用量（core#159）：SUM(size)，限定
// dir_root = ? 且该行仍存活（未软删、未过期）。
//
// ⚠️ **与站点级物理配额（GetPhysicalObjectUsage）刻意不同口径**，后人
// 「顺手对齐」成同一口径会同时毁掉两者的语义——它们回答的是**不同问题**：
//
//   - 站点配额量的是**物理占用**（按存储相对路径去重、**含回收站软删行**）
//     ⇒ 回答「盘上还占多少」。软删对象 purge 前仍在占盘，所以必须计入；
//   - 目录配额量的是**存活分享的逻辑字节** ⇒ 回答「这个目录对外分享出去
//     （且仍然有效）多少」。已软删（进回收站）与**已过期**（超过 expired_at）
//     的分享都不再计入 ⇒ 目录额度随过期/删除释放。
//
// 混用的后果是双向的：站点配额若改用本口径 ⇒ 漏算回收站占盘 ⇒ 站点配额
// 虚低、可把盘写爆；目录配额若改用物理口径 ⇒ 过期分享仍永久占着目录额度
// ⇒ 目录配额虚高、目录很快被「看不见的旧份额」锁死。
//
// 与**用户级**配额（GetTotalSizeByUserID）也不同：用户口径只排除软删行、
// 仍计入已过期未清理的分享；目录口径额外排除已过期行——目录策略是运营侧的
// 「这个目录还能对外发多少」治理项，过期分享不再消耗该目录的额度。
func (r *FileCodeRepository) GetDirectoryUsage(ctx context.Context, dirRoot string) (int64, error) {
	var usage int64
	err := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Where("dir_root = ?", dirRoot).
		// 过期行不计：expired_at 为空 = 永久分享（计入）；非空则须仍在未来。
		Where("expired_at IS NULL OR expired_at > ?", time.Now()).
		Select("COALESCE(SUM(size), 0)").Scan(&usage).Error
	// Model 查询自带软删作用域（deleted_at IS NULL）⇒ 已删行天然不计。
	return usage, err
}

func (r *FileCodeRepository) CountTodayUploads(ctx context.Context) (int64, error) {
	var count int64
	today := time.Now().Format("2006-01-02")
	err := r.db().WithContext(ctx).Model(&model.FileCode{}).Where("created_at >= ?", today).Count(&count).Error
	return count, err
}

func (r *FileCodeRepository) List(ctx context.Context, page, pageSize int, search string) ([]*model.FileCode, int64, error) {
	page, pageSize = normalizePage(page, pageSize)

	query := r.db().WithContext(ctx).Model(&model.FileCode{})

	// 搜索条件（pickup_code 纳入 LIKE 链，对齐用户面/管理端 ListWithFilter——
	// 2026-10-09 遗留缺口：管理面老 List 搜 6 位取件码不命中；NULL 列 LIKE 恒不匹配，无副作用）
	if search != "" {
		pattern, esc := LikeContains(search)
		// prefix/suffix 死列已退役（2026-10-26，库中恒空且无写入方）
		query = query.Where("code LIKE ? OR pickup_code LIKE ? OR uuid_file_name LIKE ? OR file_path LIKE ?"+esc,
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

// DeleteExpiredFiles 软删过期分享（连带软删子行「同生共死」）。
// 逐条独立事务：单条失败不阻断其余（下轮重试），与既有语义一致。
func (r *FileCodeRepository) DeleteExpiredFiles(ctx context.Context, expiredFiles []*model.FileCode) (int, error) {
	if len(expiredFiles) == 0 {
		return 0, nil
	}

	count := 0
	for _, file := range expiredFiles {
		err := r.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := softDeleteChildRows(tx, []uint{file.ID}); err != nil {
				return err
			}
			return tx.Delete(file).Error
		})
		if err != nil {
			continue // 记录错误但继续处理其他文件
		}
		count++
	}
	return count, nil
}
