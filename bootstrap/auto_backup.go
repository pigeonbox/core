package bootstrap

// 定时自动备份的装配（2026-11-06，5.1.0）。
//
// # 位置选择
//
// 与离线子命令（server/cmd/server/backup.go → NewOfflineBackupService）**同一套
// app/backup 代码**：同一个包格式、同一份 manifest 语义 ⇒ 定时产出的包能被
// `pigeonbox backup restore` 恢复，反之亦然。这里只解决「在线实例怎么拿到那些
// 依赖」以及「什么时候该跑」。
//
// # 与离线路径的差别
//
// 离线路径自建存储服务（进程独立、配置独立）；在线路径必须用**运行中的那一个**
// 存储单例——在线切换过存储后端（runtime_storage）时，只有单例知道当前真正的
// 后端。否则定时备份会去读一个没人用的后端，导出 object_count=0（2026-11-03
// 215 真机 S25d 的离线版就是这个症状，在线版不能重犯）。

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/oekit/concurrency/loop"
	"go.uber.org/zap"

	contracsErrcode "github.com/pigeonbox/contracts/errcode"
	adminApp "github.com/pigeonbox/core/app/admin"
	"github.com/pigeonbox/core/app/backup"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/resp"
	"github.com/pigeonbox/core/storage"
	"gorm.io/gorm"
)

// autoBackupCheckInterval 到期判定的轮询间隔。
//
// 为什么是「10 分钟轮询 + 到期判定」而不是「睡到明天 03:00」：
//
//	· 前者对重启/改配置/时钟跳变天然免疫（每一轮都重新看事实）；
//	· 后者一旦某次算错时机就永久错位，且改配置必须重启进程。
//
// 代价是每天最多 144 次磁盘 ListDir（几十个文件名）——可忽略。
const autoBackupCheckInterval = 10 * time.Minute

// autoBackupFirstDelay 启动后首次判定延迟：等初始化/迁移落定，避免和首启向导抢 IO。
const autoBackupFirstDelay = 2 * time.Minute

var (
	autoBackupMu     sync.Mutex
	autoBackupInited bool
	autoBackupSch    *backup.Scheduler
	autoBackupErr    error
)

// autoBackupScheduler 取（或首次构造）调度器单例。
//
// **即便 backup.enabled=false 也会构造**：管理台的「备份」卡片需要能显示目录、
// 已有包、以及「当前未启用」，并允许点一次「立即备份」先试试。Background 循环
// 才受 enabled 门控。
// initAutoBackup 装配期构造调度器（**DB / 存储由 wiring 显式传入**）。
//
// 为什么不在用的时候取**全局** DB 句柄：装配层的全局句柄消费面是只减不增的棘轮
// （internal/arch/ratchet_test.go 规则 10）。wiring 手里本来就有 database 与
// bootstrapStorage 两个实参，传进来即可，不必再开一个全局消费面。
//
// **即便 backup.enabled=false 也会构造**：管理台的「备份」卡片需要能显示目录、
// 已有包、以及「当前未启用」，并允许点一次「立即备份」先试试。后台循环才受
// enabled 门控。
func initAutoBackup(database *gorm.DB, store *storage.StorageService) {
	autoBackupMu.Lock()
	defer autoBackupMu.Unlock()
	if autoBackupInited {
		return
	}
	autoBackupInited = true
	autoBackupSch, autoBackupErr = buildAutoBackupScheduler(database, store)
}

// autoBackupScheduler 取调度器单例（未装配时如实报错，绝不 panic）。
func autoBackupScheduler() (*backup.Scheduler, error) {
	autoBackupMu.Lock()
	defer autoBackupMu.Unlock()
	if !autoBackupInited {
		return nil, fmt.Errorf("定时备份未装配（bootstrap 未调用 initAutoBackup）")
	}
	return autoBackupSch, autoBackupErr
}

// buildAutoBackupScheduler 组装调度器（依赖运行中的 DB / 存储单例 / admin 域）。
func buildAutoBackupScheduler(database *gorm.DB, store *storage.StorageService) (*backup.Scheduler, error) {
	if config == nil {
		return nil, fmt.Errorf("配置未就绪")
	}
	if database == nil {
		return nil, fmt.Errorf("数据库未就绪")
	}
	if store == nil {
		return nil, fmt.Errorf("存储未就绪")
	}
	// 对象清单来源 = admin 域（含软删记录：软删分享可恢复 ⇒ 它的对象也得备份）。
	svc := backup.NewService(database, adminApp.Default(), storageTypeAdapter{store})
	return backup.NewScheduler(svc, backup.SchedulerConfig{
		Dir:  autoBackupDir(),
		Keep: config.Backup.Keep,
		At:   config.Backup.At,
	})
}

// autoBackupDir 解析生效的备份目录（**绝对路径**）。
//
// 优先级：`backup.dir` > `<app.datapath>/backups`。相对路径**按数据目录解析**
// 而不是按进程 CWD：NAS 包 / 容器 / systemd 的 CWD 都不指向数据目录，按 CWD
// 解析会写到莫名的地方或直接 EACCES（同 preview 缓存的教训）。
func autoBackupDir() string {
	var dataPath, dir string
	// config 为空也要能答：GET /admin/diagnostics 与 GET /admin/backups 都可能
	// 在配置未就绪时被调用，nil 解引用会把「诊断」本身变成一次崩溃。
	if config != nil {
		dataPath = strings.TrimSpace(config.App.DataPath)
		dir = strings.TrimSpace(config.Backup.Dir)
	}
	if dataPath == "" {
		// ⚠️ 这是相对路径 ⇒ NewScheduler 会明确拒绝（它要求绝对路径）。
		// 有意为之：数据目录没配时，与其猜一个位置把备份写到莫名的地方，
		// 不如让状态里出现一条「备份目录未配置」，运维一眼看得见。
		dataPath = "./data"
	}
	dataPath = filepath.Clean(dataPath)
	if dir == "" {
		return filepath.Join(dataPath, "backups")
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	// 相对路径按数据目录解析（不是 CWD）；数据目录本身缺失时上面已兜成相对
	// 路径，交给 NewScheduler 拒绝。
	return filepath.Clean(filepath.Join(dataPath, dir))
}

// startAutoBackup 启动定时备份循环（仅 `backup.enabled=true` 时）。
//
// 与其它后台任务同款：GoSafe 起跑 + 共享根 ctx（停机时收尾）+ 每轮感知 ctx。
func startAutoBackup() {
	if config == nil || !config.Backup.Enabled {
		return
	}
	sch, err := autoBackupScheduler()
	if err != nil {
		logger.Warn("定时备份未启动", zap.Error(err))
		return
	}
	logger.Info("auto backup enabled",
		zap.String("dir", sch.Dir()),
		zap.Int("keep", sch.Keep()),
		zap.String("at", sch.At().String()))
	trackBackgroundLoop(loop.Run(backgroundContext(), loop.Config{
		Name:       "auto-backup",
		Interval:   autoBackupCheckInterval,
		FirstDelay: autoBackupFirstDelay,
	}, func(ctx context.Context) {
		res, err := sch.RunIfDue(ctx)
		if err != nil {
			// 已由调度器记状态 + 告警日志；这里不重复铺开。
			return
		}
		if res != nil {
			logger.Info("auto backup finished",
				zap.String("file", res.Name),
				zap.Int64("size", res.Size),
				zap.Int("pruned", len(res.Pruned)))
		}
	}))
}

// AutoBackupStatus 自动备份状态（诊断 / 管理台共用）。
type AutoBackupStatus struct {
	// Enabled 是否启用定时备份（`backup.enabled`）。
	Enabled bool "json:\"enabled\""
	// Dir 备份目录（绝对路径，便于运维直接去看）。
	Dir string "json:\"dir\""
	// Keep 保留份数（`<=0` = 不限）。
	Keep int "json:\"keep\""
	// At 每日时刻（HH:MM）。
	At string "json:\"at\""
	// Count / TotalBytes 目录里已有多少份、共多大。
	Count      int   "json:\"count\""
	TotalBytes int64 "json:\"total_bytes\""
	// LastFile / LastSize 最新一份包（按文件名时间，**含重启前的历史包**）。
	LastFile string "json:\"last_file\""
	LastSize int64  "json:\"last_size\""
	// LastBackupAt 最新一份包的生成时间（RFC3339，取自文件名，空 = 从未备份）。
	LastBackupAt string "json:\"last_backup_at\""
	// Running 当前是否正在备份。
	Running bool "json:\"running\""
	// LastError 本进程上一次失败原因（成功一次即清空）。
	LastError string "json:\"last_error\""
}

// autoBackupStatus 采集状态快照（**永不阻塞**：调度器状态用独立锁，长备份不影响）。
func autoBackupStatus() AutoBackupStatus {
	st := AutoBackupStatus{}
	if config != nil {
		st.Enabled = config.Backup.Enabled
		st.Keep = config.Backup.Keep
		st.At = config.Backup.At
		st.Dir = autoBackupDir()
	}
	sch, err := autoBackupScheduler()
	if err != nil {
		st.LastError = err.Error()
		return st
	}
	if s := sch.Status(); s.LastError != "" {
		st.LastError = s.LastError
	}
	running := sch.Status()
	st.Running = running.Running
	if running.At != "" {
		st.At = running.At
	}
	st.Dir = sch.Dir()
	st.Keep = sch.Keep()
	files, err := sch.List()
	if err != nil {
		st.LastError = err.Error()
		return st
	}
	st.Count = len(files)
	for _, f := range files {
		st.TotalBytes += f.Size
	}
	if len(files) > 0 {
		st.LastFile = files[0].Name
		st.LastSize = files[0].Size
		if ts, ok := backup.ParseFileName(files[0].Name); ok {
			st.LastBackupAt = ts.Format(time.RFC3339)
		}
	}
	return st
}

// backupListResponse 管理台备份卡片的数据（列表 + 状态一次取齐）。
type backupListResponse struct {
	AutoBackupStatus
	// Items 已备份的包（新→旧）。
	Items []backupListItem "json:\"items\""
}

// backupListItem 一份备份包。
type backupListItem struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// CreatedAt 生成时间（RFC3339；取自**文件名**而非 mtime——拷贝会改写 mtime）。
	CreatedAt string "json:\"created_at\""
	// DownloadURL 下载地址（管理面；需带 Authorization）。
	DownloadURL string "json:\"download_url\""
}

// listBackupsHandler GET /admin/backups —— 状态 + 包列表。
func listBackupsHandler(ctx context.Context, c *app.RequestContext) {
	sch, err := autoBackupScheduler()
	if err != nil {
		resp.NewErrorWithMessage(c, contracsErrcode.CodeUnavailable, err.Error())
		return
	}
	files, err := sch.List()
	if err != nil {
		resp.NewErrorWithMessage(c, contracsErrcode.CodeInternal, err.Error())
		return
	}
	out := backupListResponse{AutoBackupStatus: autoBackupStatus()}
	out.Items = make([]backupListItem, 0, len(files))
	for _, f := range files {
		item := backupListItem{
			Name:        f.Name,
			Size:        f.Size,
			DownloadURL: "/admin/backups/" + f.Name,
		}
		if ts, ok := backup.ParseFileName(f.Name); ok {
			item.CreatedAt = ts.Format(time.RFC3339)
		}
		out.Items = append(out.Items, item)
	}
	resp.Success(c, out)
}

// runBackupHandler POST /admin/backups/run —— 立即备份（后台跑，立刻返回）。
//
// 与定时任务共用单飞锁：正在跑时回 `started=false` 而不是排队/报错
// （用户点两下不该是错误）。
func runBackupHandler(ctx context.Context, c *app.RequestContext) {
	sch, err := autoBackupScheduler()
	if err != nil {
		resp.NewErrorWithMessage(c, contracsErrcode.CodeUnavailable, err.Error())
		return
	}
	if err := sch.Start(ctx); err != nil {
		// 已在跑：这不是失败，如实告知即可。
		resp.Success(c, map[string]interface{}{"started": false, "running": true})
		return
	}
	resp.Success(c, map[string]interface{}{"started": true, "running": true})
}

// downloadBackupHandler GET /admin/backups/:name —— 下载一个包。
//
// # 路径安全
//
// 名字必须先通过 backup.IsBackupFileName（固定前缀 + 固定布局时间戳 + 固定后缀，
// 且内部做了 filepath.Base）⇒ 无法拼出 `../` 或绝对路径。再叠一条「必须在当前
// 列表里」的核对：将来若有人放宽命名规则，这条仍能挡住越界读取。
func downloadBackupHandler(ctx context.Context, c *app.RequestContext) {
	name := c.Param("name")
	if !backup.IsBackupFileName(name) || filepath.Base(name) != name {
		resp.NewErrorByCode(c, contracsErrcode.CodeInvalidParam)
		return
	}
	sch, err := autoBackupScheduler()
	if err != nil {
		resp.NewErrorWithMessage(c, contracsErrcode.CodeUnavailable, err.Error())
		return
	}
	files, err := sch.List()
	if err != nil {
		resp.NewErrorWithMessage(c, contracsErrcode.CodeInternal, err.Error())
		return
	}
	found := false
	for _, f := range files {
		if f.Name == name {
			found = true
			break
		}
	}
	if !found {
		resp.NewErrorByCode(c, contracsErrcode.CodeNotFound)
		return
	}
	c.Header("Content-Type", "application/gzip")
	c.Header("Content-Disposition", "attachment; filename=\""+name+"\"")
	c.File(filepath.Join(sch.Dir(), name))
}
