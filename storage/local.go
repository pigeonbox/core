// local.go 本地后端路径语义：逃逸收口（localContain）与历史双层布局迁移
// （MigrateLegacyLayout）。
package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/pigeonbox/core/pkg/logger"
)

// localContain 本地后端路径收口：rel 清洗后必须仍落在 DataPath 根内，返回绝对路径。
// 2026-10-05 审计修复：此前仅写侧（SaveStream/SaveBytes）有逃逸守卫，读/删/Stat
// 侧裸 Join——DB 中 RelPath 被投毒（如 multi-bind 的 object_key）即可任意读/删/
// Stat 服务器文件。所有本地路径访问统一经此。
func (s *StorageService) localContain(rel string) (string, error) {
	root := filepath.Clean(s.dataPath())
	full := filepath.Clean(filepath.Join(root, rel))
	r, err := filepath.Rel(root, full)
	if err != nil || full == root || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("illegal storage path: %s", rel)
	}
	return full, nil
}

// resolveLocal 定位本地文件：DataPath+rel 单一候选。
//
// 2026-10-26 兼容层退役：此前此处有第二条候选路径 DataPath+/uploads/+rel，用于
// 兜住「统一存储实例建立前」写入的历史双层布局（config 的 storage_path 带了
// uploads 段，而存储相对路径本身又以 uploads/ 开头 ⇒ 物理路径多了一层）。
// 该布局已由 MigrateLegacyLayout 在启动期一次性上提归位，此处不再回退。
func (s *StorageService) resolveLocal(rel string) (string, bool) {
	p, err := s.localContain(rel)
	if err != nil {
		return "", false
	}
	if _, err := os.Stat(p); err == nil {
		return p, true
	}
	return p, false
}

// LocalAbsPath 本地后端下解析存储相对路径为绝对路径（仅 local 后端；远端返回空）。
// 供下载链路走 c.File（原生 Range/断点续传/MIME 推断）；路径不存在返回空。
func (s *StorageService) LocalAbsPath(rel string) string {
	if s.EffectiveType() != StorageTypeLocal {
		return ""
	}
	p, _ := s.resolveLocal(rel)
	if p == "" {
		return ""
	}
	if info, err := os.Stat(p); err != nil || info.IsDir() {
		return ""
	}
	return p
}

// MigrateLegacyLayout 迁移历史双层布局（幂等，可重复调用）。
//
// 历史形态：config 的 storage.storage_path 写成 "./data/uploads"（带了 uploads 段），
// 而存储相对路径本身又以 "uploads/" 开头 ⇒ 物理路径变成 <DataPath>/uploads/uploads/…
// 多了一层。当前形态：DataPath 是存储根，相对路径 "uploads/2026/…" 解析为
// <DataPath>/uploads/2026/…（单层）。
//
// 迁移动作：若 <DataPath>/uploads/uploads 存在且非空，把它的子项上提到
// <DataPath>/uploads/（同盘 rename，瞬时完成，不复制文件内容）。上提后旧布局目录
// 变空即删除。冲突（目标同名已存在）时跳过该项并告警，绝不覆盖已有文件。
//
// 返回 (上提的条目数, 跳过的条目数, error)。无可迁移内容时返回 (0,0,nil)。
//
// 调用时机：bootstrap 拿到存储实例后、对外服务前（读路径必须先于迁移完成）。
func MigrateLegacyLayout(dataPath string) (int, int, error) {
	if dataPath == "" {
		return 0, 0, nil
	}
	root := filepath.Clean(dataPath)
	legacy := filepath.Join(root, "uploads", "uploads")
	target := filepath.Join(root, "uploads")

	entries, err := os.ReadDir(legacy)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil // 无历史布局
		}
		return 0, 0, fmt.Errorf("read legacy layout dir: %w", err)
	}
	if len(entries) == 0 {
		// 空目录：直接清掉（残留的空壳目录本身也是历史布局的一部分）
		if err := os.Remove(legacy); err != nil {
			logger.Warn("remove empty legacy layout dir failed", zap.String("dir", legacy), zap.Error(err))
		}
		return 0, 0, nil
	}

	moved, skipped := 0, 0
	for _, e := range entries {
		from := filepath.Join(legacy, e.Name())
		to := filepath.Join(target, e.Name())
		if _, err := os.Lstat(to); err == nil {
			skipped++
			logger.Warn("legacy layout item skipped: target already exists",
				zap.String("from", from), zap.String("to", to))
			continue
		}
		if err := os.Rename(from, to); err != nil {
			skipped++
			logger.Warn("legacy layout item move failed", zap.String("from", from), zap.String("to", to), zap.Error(err))
			continue
		}
		moved++
	}
	if moved > 0 {
		logger.Info("legacy storage layout migrated", zap.String("root", root),
			zap.Int("moved", moved), zap.Int("skipped", skipped))
	}
	// 旧目录空了就清掉（非空说明有跳过项，保留以便人工处理）
	if entriesLeft, err := os.ReadDir(legacy); err == nil && len(entriesLeft) == 0 {
		if err := os.Remove(legacy); err != nil {
			logger.Warn("remove legacy layout dir failed", zap.String("dir", legacy), zap.Error(err))
		}
	}
	return moved, skipped, nil
}

// NewLocalFallback 构造本地后端兜底实例（未注入统一存储时的测试/降级路径；
// share/chunk 两域各自懒加载缓存一份，DataPath 沿用各自历史约定——
// ./data/uploads 与 ./data 的差异是 2026-10 修复后的刻意参数，勿"统一"）。
// 构造收口此处：兜底语义演进（如 BaseURL 策略）只改一点。
func NewLocalFallback(dataPath, baseURL string) *StorageService {
	return NewStorageService(&StorageConfig{
		Type:     StorageTypeLocal,
		DataPath: dataPath,
		BaseURL:  baseURL,
	})
}
