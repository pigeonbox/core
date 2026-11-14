// stats.go 统计簇：Dashboard 统计/富指标/趋势序列/传输日志查询。
package admin

import (
	"context"
	"fmt"
	"time"

	"github.com/oekit/pkg/timex"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
)

type AdminStats struct {
	TotalUsers     int64 `json:"total_users"`
	TotalFiles     int64 `json:"total_files"`
	TotalSize      int64 `json:"total_size"`
	TodayUploads   int64 `json:"today_uploads"`
	TodayDownloads int64 `json:"today_downloads"`
	ExpiredFiles   int64 `json:"expired_files"`
	AnonymousFiles int64 `json:"anonymous_files"`
	UserFiles      int64 `json:"user_files"`
	// 文件健康洞察维度（2026-10-07；IDL AdminStatsData 同步暴露）
	ActiveFiles       int64 `json:"active_files"`
	ExpiringSoonFiles int64 `json:"expiring_soon_files"`
	NeverPickedFiles  int64 `json:"never_picked_files"`
	ForeverFiles      int64 `json:"forever_files"`
}

// GetStats 获取管理员统计信息。
//
// core#137：查询失败**显式返回错误**，不再逐项静默跳过——此前坏库下恒
// `return stats, nil`，管理台统计页 200 全 0 无错误态（故障期的全 0 不是
// 统计，是假数据），与 core#102「失败必须可见」相悖。错误原文随返回值进
// handler 的 resp.NewInternalError（日志留全文、出网脱敏 + trace_id）。
func (s *Service) GetStats(ctx context.Context) (*AdminStats, error) {
	stats := &AdminStats{}

	// 获取文件统计
	totalFiles, err := s.fileCodeRepo.Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("统计 total_files: %w", err)
	}
	stats.TotalFiles = totalFiles

	// 物理口径含回收站（评审 D01）：与站点配额闸同源，purge 前软删仍占盘。
	totalSize, err := s.fileCodeRepo.GetTotalSizeIncludingDeleted(ctx)
	if err != nil {
		return nil, fmt.Errorf("统计 total_size: %w", err)
	}
	stats.TotalSize = totalSize

	todayUploads, err := s.fileCodeRepo.CountTodayUploads(ctx)
	if err != nil {
		return nil, fmt.Errorf("统计 today_uploads: %w", err)
	}
	stats.TodayUploads = todayUploads

	// 获取用户统计
	users, err := s.userRepo.Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("统计 total_users: %w", err)
	}
	stats.TotalUsers = users

	// 获取今日下载（从 transfer_log 统计）
	// todayDownloads, err := s.transferLogRepo.CountTodayDownloads(ctx)
	// if err == nil {
	// 	stats.TodayDownloads = todayDownloads
	// }

	// 统计匿名上传和用户上传
	// anonymousFiles, _ := s.fileCodeRepo.CountByUploadType(ctx, "anonymous")
	// userFiles, _ := s.fileCodeRepo.CountByUploadType(ctx, "authenticated")
	// stats.AnonymousFiles = anonymousFiles
	// stats.UserFiles = userFiles

	// 文件健康洞察（2026-10-07 对标上游 dashboard 洞察卡）。
	// core#137：原「失败降级为 0，不阻塞统计」同口径收回——单项假 0 与整页
	// 假 0 是同一个谎言，只是规模不同。
	active, expired, expiringSoon, neverPicked, forever, err := s.fileCodeRepo.CountByHealth(ctx)
	if err != nil {
		return nil, fmt.Errorf("统计 file_health: %w", err)
	}
	stats.ActiveFiles = active
	stats.ExpiredFiles = expired
	stats.ExpiringSoonFiles = expiringSoon
	stats.NeverPickedFiles = neverPicked
	stats.ForeverFiles = forever

	return stats, nil
}

// GetTransferLogs 获取传输日志
func (s *Service) GetTransferLogs(ctx context.Context, query model.TransferLogQuery) ([]*model.TransferLog, int64, error) {
	return s.transferLogRepo.List(ctx, query)
}

// ListAdminOperationLogs 查询管理操作审计（gen/handler 消费面收口，handler 不直依 dao）。
func (s *Service) ListAdminOperationLogs(ctx context.Context, query model.AdminOperationLogQuery) ([]*model.AdminOperationLog, int64, error) {
	return s.adminOperationRepo.List(ctx, query)
}

// StorageInsight 存储洞察聚合（gen/handler/storage 洞察端点消费面收口）。
func (s *Service) StorageInsight(ctx context.Context) (total, totalSize, alive, softDeleted int64, err error) {
	return s.fileCodeRepo.InsightStats(ctx)
}

// TransferTrendByDay 传输次数按天趋势（gen/handler/storage 今日流量消费面收口；
// operation 空 = 全部类型）。
func (s *Service) TransferTrendByDay(ctx context.Context, from time.Time, operation string) ([]dao.DayCount, error) {
	return s.transferLogRepo.TrendByDay(ctx, from, operation)
}

// EnhancedStats Dashboard 富指标（昨日对比/下载总量/类型分布）。
type EnhancedStats struct {
	TodayUploads     int64             `json:"today_uploads"`
	YesterdayUploads int64             `json:"yesterday_uploads"`
	TotalDownloads   int64             `json:"total_downloads"`
	ExpiredFiles     int64             `json:"expired_files"`
	AnonymousFiles   int64             `json:"anonymous_files"`
	PresignFiles     int64             `json:"presign_files"`
	TopSuffixes      []*dao.SuffixStat `json:"top_suffixes"`
	// 站点级存储配额（storage.quota，0=不限）与当前用量（物理口径，含回收站软删）
	StorageUsed  int64 `json:"storage_used"`
	StorageQuota int64 `json:"storage_quota"`
}

// EnhancedStats 汇总富指标。
//
// core#139：任一查询失败**显式返回错误**，不再单项静默降级 0——坏库下 200
// 全 0（或部分 0）是假数据且无错误态，与 core#102「失败必须可见」相悖。
// TAIL2（core#137）已把 GetStats/GetConfig 同口径收口，本项是同族最后一处；
// 错误原文随返回值进 handler 的 resp.NewInternalError（日志留全文、出网
// 脱敏 + trace_id）。
func (s *Service) EnhancedStats(ctx context.Context) (*EnhancedStats, error) {
	now := time.Now()
	todayStart := timex.BeginningOfDay(now)
	yesterdayStart := todayStart.Add(-24 * time.Hour)

	stats := &EnhancedStats{}
	todayUploads, err := s.fileCodeRepo.CountCreatedBetween(ctx, todayStart, now.Add(time.Minute))
	if err != nil {
		return nil, fmt.Errorf("统计 today_uploads: %w", err)
	}
	stats.TodayUploads = todayUploads

	yesterdayUploads, err := s.fileCodeRepo.CountCreatedBetween(ctx, yesterdayStart, todayStart)
	if err != nil {
		return nil, fmt.Errorf("统计 yesterday_uploads: %w", err)
	}
	stats.YesterdayUploads = yesterdayUploads

	totalDownloads, err := s.fileCodeRepo.SumUsedCount(ctx)
	if err != nil {
		return nil, fmt.Errorf("统计 total_downloads: %w", err)
	}
	stats.TotalDownloads = totalDownloads

	expiredFiles, err := s.fileCodeRepo.CountExpired(ctx)
	if err != nil {
		return nil, fmt.Errorf("统计 expired_files: %w", err)
	}
	stats.ExpiredFiles = expiredFiles

	anonymousFiles, err := s.fileCodeRepo.CountByUploadType(ctx, "anonymous")
	if err != nil {
		return nil, fmt.Errorf("统计 anonymous_files: %w", err)
	}
	stats.AnonymousFiles = anonymousFiles

	presignFiles, err := s.fileCodeRepo.CountByUploadType(ctx, "presign_anonymous")
	if err != nil {
		return nil, fmt.Errorf("统计 presign_files: %w", err)
	}
	presignAuthFiles, err := s.fileCodeRepo.CountByUploadType(ctx, "presign_authenticated")
	if err != nil {
		return nil, fmt.Errorf("统计 presign_files: %w", err)
	}
	stats.PresignFiles = presignFiles + presignAuthFiles

	// ⚠️ 2026-10-26：此处曾把错误吞掉（`_`）⇒ 「文件类型分布」恒为空且**零日志**
	// （退役 suffix 列后 SQL 直接报错，没人知道）。core#139 起失败显式返回
	// （此前只记 warn 仍降级空列表 = 单项假数据）。
	topSuffixes, err := s.fileCodeRepo.TopSuffixes(ctx, 10)
	if err != nil {
		return nil, fmt.Errorf("统计 top_suffixes: %w", err)
	}
	stats.TopSuffixes = topSuffixes

	// 物理口径含回收站（评审 D01）：StorageUsed 与站点配额闸同源。
	storageUsed, err := s.fileCodeRepo.GetTotalSizeIncludingDeleted(ctx)
	if err != nil {
		return nil, fmt.Errorf("统计 storage_used: %w", err)
	}
	stats.StorageUsed = storageUsed
	if cfg := conf.GetGlobalConfig(); cfg != nil {
		stats.StorageQuota = cfg.Storage.Quota
	}
	return stats, nil
}

// TrendDay 趋势序列单日点。
type TrendDay struct {
	Date      string `json:"date"`
	Uploads   int64  `json:"uploads"`
	Downloads int64  `json:"downloads"`
}

// TrendSeries 连续 N 天的上传/下载趋势（缺失日补 0）。下载日志查询失败不阻断
// （表可能尚无数据/旧库未建）——降级为全 0 序列。
func (s *Service) TrendSeries(ctx context.Context, days int) ([]TrendDay, error) {
	now := time.Now()
	todayStart := timex.BeginningOfDay(now)
	from := todayStart.AddDate(0, 0, -(days - 1))

	upRows, err := s.fileCodeRepo.TrendByDay(ctx, from)
	if err != nil {
		return nil, err
	}
	dlRows, _ := s.transferLogRepo.TrendByDay(ctx, from, "download")

	upMap := make(map[string]int64, len(upRows))
	for _, r := range upRows {
		upMap[r.Date] = r.Count
	}
	dlMap := make(map[string]int64, len(dlRows))
	for _, r := range dlRows {
		dlMap[r.Date] = r.Count
	}

	series := make([]TrendDay, 0, days)
	for i := 0; i < days; i++ {
		date := from.AddDate(0, 0, i).Format("2006-01-02")
		series = append(series, TrendDay{Date: date, Uploads: upMap[date], Downloads: dlMap[date]})
	}
	return series, nil
}
