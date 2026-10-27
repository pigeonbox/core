// system.go 系统信息/日志簇：运行时信息/存储状态/系统日志查询。
package admin

import (
	"context"
	"runtime"
	"time"

	"github.com/oekit/core/version"
	"github.com/pigeonbox/core/internal/paging"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/storage"
)

// SystemInfo 系统信息
type SystemInfo struct {
	Version     string `json:"version"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Uptime      string `json:"uptime"`
	Goroutines  int64  `json:"goroutines"`
	MemoryAlloc int64  `json:"memory_alloc"`
	MemoryTotal int64  `json:"memory_total"`
	MemorySys   int64  `json:"memory_sys"`
}

// GetSystemInfo 获取系统信息。版本/进程启动时刻来自 kit/version（-ldflags
// 注入，未注入时 version=dev、StartTime=包加载时刻）——此前 Version 硬编码
// "1.0.0"、Uptime 用 time.Since(time.Now()) 恒为零，均为假数据。
func (s *Service) GetSystemInfo(ctx context.Context) (*SystemInfo, error) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	uptime := time.Since(version.StartTime).Truncate(time.Second)

	return &SystemInfo{
		Version:     version.Version,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Uptime:      uptime.String(),
		Goroutines:  int64(runtime.NumGoroutine()),
		MemoryAlloc: int64(mem.Alloc),
		MemoryTotal: int64(mem.TotalAlloc),
		MemorySys:   int64(mem.Sys),
	}, nil
}

// StorageStatus 存储状态
type StorageStatus struct {
	StorageType  string  `json:"storage_type"`
	TotalSpace   int64   `json:"total_space"`
	UsedSpace    int64   `json:"used_space"`
	FreeSpace    int64   `json:"free_space"`
	FileCount    int64   `json:"file_count"`
	UsagePercent float64 `json:"usage_percent"`
}

// GetStorageStatus 获取存储状态（真实数据）。
//   - 后端类型：注入的 *storage.StorageService.EffectiveType()（远端构造失败
//     降级 local 时如实报 local）；未注入/非具体类型时按 local。
//   - 容量：仅本地后端有磁盘容量语义（Statfs 实测总/可用）；对象存储等远端
//     后端容量弹性无上限，Total/Free 如实置 0，不虚构数字（此前恒报
//     local+100GB 假容量，管理面板展示的是虚构信息）。
//   - UsedSpace/FileCount：DB 存活分享的尺寸合计/条数（应用层口径，与
//     站点配额闸同源），本地模式下不等于磁盘已用（磁盘还含分片临时区等）。
func (s *Service) GetStorageStatus(ctx context.Context) (*StorageStatus, error) {
	totalFiles, err := s.fileCodeRepo.Count(ctx)
	if err != nil {
		return nil, err
	}
	totalSize, err := s.fileCodeRepo.GetTotalSize(ctx)
	if err != nil {
		return nil, err
	}

	storageType := string(storage.StorageTypeLocal)
	dataPath := ""
	if svc, ok := s.storage.(*storage.StorageService); ok && svc != nil {
		storageType = string(svc.EffectiveType())
		dataPath = svc.DataPath()
	}

	st := &StorageStatus{
		StorageType: storageType,
		UsedSpace:   totalSize,
		FileCount:   totalFiles,
	}
	if storageType == string(storage.StorageTypeLocal) && dataPath != "" {
		if total, free, derr := storage.DiskUsage(dataPath); derr == nil {
			st.TotalSpace = int64(total)
			st.FreeSpace = int64(free)
			if total > 0 {
				st.UsagePercent = float64(totalSize) / float64(total) * 100
			}
		}
		// Statfs 失败（路径不可达等）保持 0 值如实呈现，不虚构
	}
	return st, nil
}

// LogEntry 日志条目
type LogEntry struct {
	ID        uint64 `json:"id"`
	Level     string `json:"level"`
	Message   string `json:"message"`
	CreatedAt string `json:"created_at"`
	Module    string `json:"module"`
	UserID    string `json:"user_id"`
}

// normalizeLogPage 系统日志分页参数归一化（默认 50、上限 500、超限截断）。
//
// 与其它域口径不同（默认 50 / 上限 500），是既有契约（响应条数对调用方可见）
// ⇒ 口径表见 internal/paging。
func normalizeLogPage(page, pageSize int) (int, int) {
	return paging.AdminSystemLogPolicy.Normalize(page, pageSize)
}

// GetSystemLogs 获取系统日志（真实数据）：读 pkg/logger 进程内环形缓冲
// （Init 时 Tee 进主日志链，容量有界、重启即清——定位是「最近运行日志」
// 面板；操作审计在 operation_logs 表另有专页）。此前是返回模拟数据的
// TODO 桩。level 空=全部，否则精确匹配级别名；分页窗口按时间序最旧在前。
func (s *Service) GetSystemLogs(ctx context.Context, level string, page, pageSize int) ([]*LogEntry, int64, error) {
	page, pageSize = normalizeLogPage(page, pageSize)

	entries, total := logger.RecentLogs(level, (page-1)*pageSize, pageSize)
	logs := make([]*LogEntry, 0, len(entries))
	for _, e := range entries {
		logs = append(logs, &LogEntry{
			ID:        e.Seq,
			Level:     e.Level,
			Message:   e.Message,
			CreatedAt: e.Time.Format("2006-01-02 15:04:05"),
			Module:    e.Module,
			UserID:    "", // 运行日志不携带用户维度；用户操作追溯走审计日志页
		})
	}
	return logs, int64(total), nil
}
