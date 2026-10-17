// stats.go 统计簇：Dashboard 统计/富指标/趋势序列/传输日志查询。
package admin

import (
	"context"
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

// GetStats 获取管理员统计信息
func (s *Service) GetStats(ctx context.Context) (*AdminStats, error) {
	stats := &AdminStats{}

	// 获取文件统计
	totalFiles, err := s.fileCodeRepo.Count(ctx)
	if err == nil {
		stats.TotalFiles = totalFiles
	}

	totalSize, err := s.fileCodeRepo.GetTotalSize(ctx)
	if err == nil {
		stats.TotalSize = totalSize
	}

	todayUploads, err := s.fileCodeRepo.CountTodayUploads(ctx)
	if err == nil {
		stats.TodayUploads = todayUploads
	}

	// 获取用户统计
	users, err := s.userRepo.Count(ctx)
	if err == nil {
		stats.TotalUsers = users
	}

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

	// 文件健康洞察（2026-10-07 对标上游 dashboard 洞察卡；失败降级为 0，不阻塞统计）
	if active, expired, expiringSoon, neverPicked, forever, err := s.fileCodeRepo.CountByHealth(ctx); err == nil {
		stats.ActiveFiles = active
		stats.ExpiredFiles = expired
		stats.ExpiringSoonFiles = expiringSoon
		stats.NeverPickedFiles = neverPicked
		stats.ForeverFiles = forever
	}

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
	// 站点级存储配额（storage.quota，0=不限）与当前用量（存活 file_codes 合计）
	StorageUsed  int64 `json:"storage_used"`
	StorageQuota int64 `json:"storage_quota"`
}

// EnhancedStats 汇总富指标。单项统计失败按 0 降级（与历史 handler 行为一致）。
func (s *Service) EnhancedStats(ctx context.Context) (*EnhancedStats, error) {
	now := time.Now()
	todayStart := timex.BeginningOfDay(now)
	yesterdayStart := todayStart.Add(-24 * time.Hour)

	stats := &EnhancedStats{}
	stats.TodayUploads, _ = s.fileCodeRepo.CountCreatedBetween(ctx, todayStart, now.Add(time.Minute))
	stats.YesterdayUploads, _ = s.fileCodeRepo.CountCreatedBetween(ctx, yesterdayStart, todayStart)
	stats.TotalDownloads, _ = s.fileCodeRepo.SumUsedCount(ctx)
	stats.ExpiredFiles, _ = s.fileCodeRepo.CountExpired(ctx)
	stats.AnonymousFiles, _ = s.fileCodeRepo.CountByUploadType(ctx, "anonymous")
	presignFiles, _ := s.fileCodeRepo.CountByUploadType(ctx, "presign_anonymous")
	presignAuthFiles, _ := s.fileCodeRepo.CountByUploadType(ctx, "presign_authenticated")
	stats.PresignFiles = presignFiles + presignAuthFiles
	stats.TopSuffixes, _ = s.fileCodeRepo.TopSuffixes(ctx, 10)
	stats.StorageUsed, _ = s.fileCodeRepo.GetTotalSize(ctx)
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
