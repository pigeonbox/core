// NAS 本地文件管理（对标上游 2.7.0 data/local 管理端）：白名单目录的列表/删除/导入。
// 与本地导入共用 upload.local_import 配置（enabled + roots）。
// 路径一律以「root 索引 + 白名单内相对路径」表达：客户端永不传绝对路径，
// root 取自服务端配置，杜绝目录穿越与任意路径删除。
package share

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/oekit/pkg/pathx"

	"github.com/pigeonbox/core/conf"
)

// LocalFileEntry 本地文件列表项（Path 为相对 root 的路径，正斜杠）
type LocalFileEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	IsDir   bool      `json:"is_dir"`
}

// resolveLocalPath 把 (rootIdx, relPath) 解析为白名单内的绝对路径。
// 防穿越三层：root 只来自服务端配置（客户端传索引）→ relPath 拒绝绝对路径与
// ".." 段 → EvalSymlinks 后复核落点仍在 root 内（防符号链接逃逸）。
func resolveLocalPath(rootIdx int, relPath string) (string, error) {
	cfg := conf.GetGlobalConfig()
	if cfg == nil || !cfg.Upload.LocalImport.Enabled {
		return "", errors.New("本地文件功能未启用")
	}
	roots := cfg.Upload.LocalImport.Roots
	// 启用但未配白名单目录：报引导性错误（管理端据此展示配置引导而非通用报错）；
	// 与「索引越界」（已配 roots 但客户端传了不存在的序号）分级
	if len(roots) == 0 {
		return "", errors.New("本地导入已启用但未配置白名单目录（roots）")
	}
	if rootIdx < 0 || rootIdx >= len(roots) {
		return "", errors.New("root 索引越界")
	}
	rootReal, err := filepath.EvalSymlinks(filepath.Clean(roots[rootIdx]))
	if err != nil {
		return "", fmt.Errorf("根目录不可访问: %w", err)
	}
	if relPath == "" || relPath == "." {
		return rootReal, nil
	}
	// 相对路径收口用 oekit/pkg/pathx 的严格档：统一把 "\" 当分隔符（Windows 风格
	// 载荷在 Linux 上按平台解释会漏判——而 Linux 才是服务端主环境），并拒绝绝对
	// 路径/UNC/盘符/显式 ".." 段。此前是「路径里含 '..' 就算非法」的字符串判定：
	// 连带误杀合法名（含 ".." 的普通文件名），且对 "C:\..." 这类形态没有结论。
	cleanRel, ok := pathx.SanitizeRelStrict(relPath)
	if !ok {
		return "", errors.New("非法路径")
	}
	abs := filepath.Join(rootReal, filepath.FromSlash(cleanRel))
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("路径不可访问: %w", err)
	}
	// 符号链接解析后的真实路径仍须落在根内（pathx 只看文本，不解析符号链接）
	if real != rootReal && !strings.HasPrefix(real, rootReal+string(filepath.Separator)) {
		return "", errors.New("路径越界")
	}
	return real, nil
}

// LocalImportRoots 返回已配置白名单根目录（已 EvalSymlinks；未启用时为空）
func LocalImportRoots() []string {
	cfg := conf.GetGlobalConfig()
	if cfg == nil || !cfg.Upload.LocalImport.Enabled {
		return nil
	}
	out := make([]string, 0, len(cfg.Upload.LocalImport.Roots))
	for _, r := range cfg.Upload.LocalImport.Roots {
		if real, err := filepath.EvalSymlinks(filepath.Clean(r)); err == nil {
			out = append(out, real)
		}
	}
	return out
}

// ListLocalFiles 列出 root 下（可选子目录 relDir）一层的条目（目录优先、按名排序）
func (s *Service) ListLocalFiles(rootIdx int, relDir string) ([]LocalFileEntry, error) {
	dir, err := resolveLocalPath(rootIdx, relDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取目录失败: %w", err)
	}
	out := make([]LocalFileEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue // 并发删除等竞态下跳过
		}
		rel := e.Name()
		if relDir != "" && relDir != "." {
			rel = filepath.ToSlash(filepath.Join(relDir, e.Name()))
		}
		out = append(out, LocalFileEntry{
			Name: e.Name(), Path: rel,
			Size: info.Size(), ModTime: info.ModTime(), IsDir: e.IsDir(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// DeleteLocalFile 删除白名单内的单个普通文件（拒绝目录与非普通文件）
func (s *Service) DeleteLocalFile(rootIdx int, relPath string) error {
	real, err := resolveLocalPath(rootIdx, relPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(real)
	if err != nil {
		return fmt.Errorf("文件不可访问: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("仅允许删除普通文件")
	}
	if err := os.Remove(real); err != nil {
		return fmt.Errorf("删除失败: %w", err)
	}
	return nil
}

// LocalImportAbsPath 把 (rootIdx, relPath) 解析为导入用的绝对路径（供管理端导入复用
// ImportLocalFile；ImportLocalFile 内部会再做一次全量校验）
func LocalImportAbsPath(rootIdx int, relPath string) (string, error) {
	return resolveLocalPath(rootIdx, relPath)
}
