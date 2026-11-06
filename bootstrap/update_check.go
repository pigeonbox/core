package bootstrap

// 更新检查的装配（2026-11-06，5.0.0）。
//
// 这里只做三件事：从生效配置取开关与地址、把缓存落在**已解析的数据目录**下、
// 把当前构建版本传进去。检查逻辑本身在 core/app/updatecheck（可单测、无全局依赖）。
//
// # 为什么缓存必须落数据目录
//
// 与 preview 缓存同一个教训（bootstrap/preview_cache_path_test.go）：相对路径按
// **进程 CWD** 解析，而 NAS 包 / 容器 / systemd 的 CWD 都不指向数据目录
// ⇒ 建目录或写文件 EACCES。区别是预览那样会**功能整体不可用**，这里是
// **静默不缓存** ⇒ 每次打开管理台都打一次 GitHub ⇒ 很快撞上未认证配额
// （60 次/小时）。所以缓存路径为空时宁可不落盘，也不要用相对路径。

import (
	"context"
	"path/filepath"
	"strings"
	"sync"

	"github.com/oekit/core/version"

	"github.com/pigeonbox/core/app/updatecheck"
)

var (
	updateCheckOnce   sync.Once
	updateCheckerInst *updatecheck.Checker
)

// initUpdateCheck 构造更新检查器（幂等）。
//
// 只在首次调用时读配置：更新检查的开关与地址来自 yaml / env，
// 不在管理台的可编辑段里（改了要重启），所以不需要热更新。
func initUpdateCheck() {
	updateCheckOnce.Do(func() {
		cfg := updatecheck.Config{
			Enabled: true,
			Current: version.Version,
		}
		if config != nil {
			cfg.Enabled = config.Update.CheckEnabled
			if ep := strings.TrimSpace(config.Update.Endpoint); ep != "" {
				cfg.Endpoint = ep
			}
			// 落盘缓存（纯内存兜底：数据目录不可写时只是不缓存，功能不受影响）。
			if dp := strings.TrimSpace(config.App.DataPath); dp != "" {
				cfg.CachePath = filepath.Join(dp, "update-check.json")
			}
		}
		updateCheckerInst = updatecheck.New(cfg)
	})
}

// updateCheckResult 返回更新状态（未初始化时退化为 unknown，不 panic）。
func updateCheckResult(ctx context.Context) updatecheck.Result {
	initUpdateCheck()
	if updateCheckerInst == nil {
		return updatecheck.Result{Current: version.Version, Source: updatecheck.SourceUnknown}
	}
	return updateCheckerInst.Check(ctx, false)
}
