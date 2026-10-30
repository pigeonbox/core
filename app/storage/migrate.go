package storage

// 存储迁移：把**存量对象**从当前后端搬到目标后端。
//
// # 为什么需要
//
// 换后端 / 换机器 /「NAS 小盘迁对象存储」此前只有一条路：管理台**直接切后端**
// ⇒ 切完之前的历史对象**留在旧后端**，而新写入落在新后端 ⇒ 旧分享取不到文件。
// （`SwitchStorage` 只切"新写入去哪"，不搬"存量在哪"。）
//
// # 三条语义（缺一条就危险）
//
//  1. **取件码与分享链接不变**：DB 记录一个字节都不动，只换对象所在的后端
//     ⇒ 迁移不是"重新分享"，用户的对外名片（码 / 链接）保持有效。
//  2. **全部复制成功后才切换**：任何对象失败 ⇒ **不切换** ⇒ 源后端原样在用，
//     线上零影响（失败只留下目标后端里的垃圾对象，重跑会覆盖）。
//  3. **可中断可续**：目标已存在的对象跳过 ⇒ 大实例中途断了重跑即可，
//     不用从头再来。
//
// # 分层（保持域间零耦合）
//
// 存储域**不直接碰 DB**（与「存储域不直接碰 system_configs」同款纪律）：
// 待迁移的对象清单由 admin 域通过 `ObjectLister` 注入；
// 读取源对象靠运行时的 `ObjectReader`（core/storage 单例实现）。

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	corestorage "github.com/pigeonbox/core/storage"
)

// ObjectLister 列出被 DB 引用的对象路径（admin 域实现）。
//
// 路径是**存储相对路径**（与 file_codes.file_path / file_code_files.file_path 同形）。
type ObjectLister interface {
	// ListReferencedObjectPaths 返回全部被引用的对象路径（去重）。
	// 必须含**软删**记录：软删分享可恢复 ⇒ 它的对象也得搬过去。
	ListReferencedObjectPaths(ctx context.Context) ([]string, error)
}

// ObjectReader 读对象（当前生效后端）。
//
// 由 core/storage 的单例实现（它同时是 Runtime）；用类型断言而非接口继承，
// 免得为迁移去改 Runtime 的既有契约（别的实现者不必跟着改）。
type ObjectReader interface {
	// GetFileReader 打开对象读取器；同时给出对象大小（远端驱动写流需确切 size）。
	GetFileReader(ctx context.Context, filePath string) (io.ReadCloser, int64, error)
	// FileExists 对象是否已存在（续跑跳过判定）。
	FileExists(ctx context.Context, filePath string) bool
}

// MigrateStorageReq 迁移请求（域内形态；契约层同名结构见 contracts 的 idl/storage.thrift）。
type MigrateStorageReq struct {
	// TargetType 目标后端类型（local / s3 / webdav / …）。
	TargetType string
	// TargetPath 目标本地目录（仅 TargetType=local 时有意义；缺省 = 沿用现配置的 storage_path）。
	TargetPath string
	// DryRun 只算计划不搬（先看再动）。
	DryRun bool
}

// MigrateStorageResult 迁移结果。
type MigrateStorageResult struct {
	// Switched 是否已切换到目标后端。
	Switched bool
	// Copied 本次复制成功的对象数。
	Copied int64
	// Skipped 目标已存在而跳过的对象数（续跑）。
	Skipped int64
	// Failed 复制失败的对象数（>0 ⇒ 未切换）。
	Failed int64
	// Bytes 复制字节数。
	Bytes int64
	// Objects 计划涉及的对象总数。
	Objects int64
	// FromType 源后端类型。
	FromType string
	// ToType 目标后端类型。
	ToType string
	// Message 人类可读结论（失败摘要 / dry-run 计划）。
	Message string
}

// supportedStorageTypes 支持的存储后端类型（与 SwitchStorage 同款白名单）。
var supportedStorageTypes = map[string]bool{
	"local": true, "s3": true, "webdav": true, "oss": true, "cos": true,
	"bos": true, "ks3": true, "obs": true, "gcs": true, "ftp": true,
	"sftp": true, "azureblob": true, "hdfs": true, "onedrive": true,
}

// SetObjectLister 注入对象清单来源（admin 域实现；bootstrap 装配）。
func (s *Service) SetObjectLister(l ObjectLister) { s.objectLister = l }

// MigrateStorage 把存量对象从当前后端搬到目标后端。
//
// 流程：校验目标 → 探测目标连通性 → 列举待迁对象 →（dry-run 则到此为止）→
// 逐个复制（目标已存在则跳过）→ 全部成功才切换后端。
//
// 失败语义：**不切换**（源后端继续在用），并返回失败摘要。
func (s *Service) MigrateStorage(ctx context.Context, req *MigrateStorageReq) (*MigrateStorageResult, error) {
	if req == nil || strings.TrimSpace(req.TargetType) == "" {
		return nil, fmt.Errorf("目标存储类型不能为空")
	}
	if !supportedStorageTypes[req.TargetType] {
		return nil, fmt.Errorf("不支持的存储类型: %s", req.TargetType)
	}
	if s.runtime == nil {
		return nil, fmt.Errorf("存储运行时未就绪")
	}
	if s.objectLister == nil {
		return nil, fmt.Errorf("存储迁移未装配对象清单来源（objectLister 缺失）")
	}
	reader, ok := s.runtime.(ObjectReader)
	if !ok {
		return nil, fmt.Errorf("当前存储运行时不支持对象读取（无法迁移）")
	}

	fromType := string(s.runtime.EffectiveType())
	candidate := s.candidateFor(req.TargetType)
	if req.TargetPath != "" {
		if req.TargetType != "local" {
			return nil, fmt.Errorf("target_path 只对 local 后端有效（当前目标: %s）", req.TargetType)
		}
		candidate.StoragePath = req.TargetPath
	}
	// 同源拒绝：搬到自己身上没有意义（还会把对象复制到同路径，风险大）
	if candidate.Type == fromType {
		if candidate.Type != "local" || samePath(candidate.StoragePath, s.getStoragePath()) {
			return nil, fmt.Errorf("目标后端与当前后端相同（%s）⇒ 无需迁移", fromType)
		}
	}

	// 目标连通性探测（与切换同款：认证级 Probe，失败不碰现有配置）
	targetCfg := corestorage.ConfigFromConf(candidate, s.baseURL())
	if err := corestorage.ProbeConfig(ctx, targetCfg); err != nil {
		return nil, fmt.Errorf("目标后端连通性验证失败: %w", err)
	}

	paths, err := s.objectLister.ListReferencedObjectPaths(ctx)
	if err != nil {
		return nil, fmt.Errorf("列举待迁移对象失败: %w", err)
	}

	res := &MigrateStorageResult{
		FromType: fromType,
		ToType:   candidate.Type,
		Objects:  int64(len(paths)),
	}
	if req.DryRun {
		res.Message = fmt.Sprintf("计划迁移 %d 个对象（%s → %s），未执行", len(paths), fromType, candidate.Type)
		return res, nil
	}

	// 目标实例（独立于当前生效实例：复制期间源后端必须原样在用）
	target, err := corestorage.NewStorageServiceE(targetCfg)
	if err != nil {
		return nil, fmt.Errorf("构造目标存储实例失败: %w", err)
	}

	var errs []string
	for _, p := range paths {
		if target.FileExists(ctx, p) {
			res.Skipped++ // 续跑：目标已有 ⇒ 跳过
			continue
		}
		rc, size, rerr := reader.GetFileReader(ctx, p)
		if rerr != nil {
			res.Failed++
			errs = append(errs, fmt.Sprintf("%s: 读取失败: %v", p, rerr))
			continue
		}
		// expectedSize=源侧大小 ⇒ 远端驱动按确切长度写（源被截断会当场失败）
		n, werr := target.SaveStream(ctx, p, rc, size)
		_ = rc.Close()
		if werr != nil {
			res.Failed++
			errs = append(errs, fmt.Sprintf("%s: 写入失败: %v", p, werr))
			continue
		}
		res.Copied++
		res.Bytes += n
	}

	if res.Failed > 0 {
		res.Message = fmt.Sprintf("%d 个对象迁移失败（已中止，源后端仍在用，目标里的残留可重跑覆盖）: %s",
			res.Failed, strings.Join(truncateStrings(errs, 5), "; "))
		return res, fmt.Errorf("%s", res.Message)
	}

	// 全部成功 ⇒ 切换（复用 activate：校验 → Probe → 热重载 → 持久化）
	if err := s.activate(ctx, candidate); err != nil {
		res.Message = "对象已复制完成，但切换后端失败（源后端仍在用）: " + err.Error()
		return res, fmt.Errorf("%s", res.Message)
	}
	res.Switched = true
	res.Message = fmt.Sprintf("已迁移 %d 个对象（%d 个目标已存在跳过，%d 字节），并切换到 %s",
		res.Copied, res.Skipped, res.Bytes, candidate.Type)
	return res, nil
}

// samePath 两个路径是否指向同一目录（相对路径按当前工作目录归一）。
func samePath(a, b string) bool {
	na, errA := filepath.Abs(a)
	nb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return na == nb
}

// truncateStrings 截断错误列表（只留前 n 条，避免把几百个失败路径全塞进响应）。
func truncateStrings(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	out := make([]string, 0, n+1)
	out = append(out, items[:n]...)
	out = append(out, fmt.Sprintf("（另有 %d 条省略）", len(items)-n))
	return out
}
