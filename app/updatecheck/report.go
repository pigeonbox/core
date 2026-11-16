package updatecheck

// 匿名安装分布上报（hub#385）——本地优先的**可选**半边。
//
// # 为什么默认关闭
//
// 「安全修复到达率」这个问题只能靠聚合安装版本分布回答，但**采集本身是
// 产品决策**：默认上报会把所有自托管实例变成数据源。故本实现：
//   - ReportURL 为空（默认）= 完全不上报，代码路径直接返回；
//   - 开启后也只发送 reportPayload 的七个字段（版本/平台/架构/升级跨度/
//     schema 版本/实例随机 ID/检查时间）——不含站点域名、用户、分享内容；
//   - 实例 ID 是数据目录下的 16 字节随机数，删文件即重置，与任何身份无关。
//
// 字段清单是**契约**：新增字段必须先改 docs/DIAGNOSTICS.md 的隐私表——
// 否则「文档说只采集七个字段」会悄悄变成谎言。
//
// # 失败姿态
//
// fire-and-forget：上报失败不影响检查结果、不重试、不打日志噪声。
// 它只是「让维护者看见分布」，不是业务链路的一部分。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/oekit/concurrency/async"
)

// reportPayload 匿名上报的**完整**字段清单（契约，见 docs/DIAGNOSTICS.md）。
type reportPayload struct {
	// Version 当前构建版本（与 /version 同源）。
	Version string `json:"version"`
	// Platform 运行平台（runtime.GOOS）。
	Platform string `json:"platform"`
	// Arch 运行架构（runtime.GOARCH）。
	Arch string `json:"arch"`
	// UpgradeFrom 上一次运行的构建版本（全新安装为空）。
	UpgradeFrom string `json:"upgrade_from,omitempty"`
	// SchemaVersion 已应用的迁移版本（未启用版本化迁移时为空）。
	SchemaVersion string `json:"schema_version,omitempty"`
	// InstanceID 安装实例随机标识（删数据目录下 instance-id 即重置）。
	InstanceID string `json:"instance_id"`
	// CheckedAt 本次检查时间（服务端聚合时用于活跃度口径）。
	CheckedAt time.Time `json:"checked_at"`
}

// reportInstall 异步上报一次安装分布；ReportURL 为空即完全跳过。
func (c *Checker) reportInstall(at time.Time) {
	url := strings.TrimSpace(c.cfg.ReportURL)
	if url == "" {
		return
	}
	if err := validateEndpoint(url); err != nil {
		return
	}
	payload := reportPayload{
		Version:       c.cfg.Current,
		Platform:      runtime.GOOS,
		Arch:          runtime.GOARCH,
		UpgradeFrom:   c.cfg.UpgradeFrom,
		SchemaVersion: c.cfg.SchemaVersion,
		InstanceID:    c.cfg.InstanceID,
		CheckedAt:     at,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	// GoSafe：上报协程 panic 不能打死进程（本包「永不 panic」约束的延伸）。
	async.GoSafeLabeled("updatecheck-report", func() {
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", c.cfg.UserAgent+"/"+strings.TrimPrefix(c.cfg.Current, "v"))
		resp, err := c.client.Do(req)
		if err != nil {
			return
		}
		_ = resp.Body.Close()
	})
}
