package bootstrap

// 安装指纹（hub#385）——本地优先：给诊断包与可选匿名上报提供
// 「版本 / 平台 / 架构 / 升级跨度 / schema 版本 / 实例标识」六个事实。
//
// # 为什么写文件而不是只读进程内状态
//
// 「升级跨度」（upgrade_from）在进程内天然拿不到：新进程启动时只知道
// **现在**是哪个版本。要回答「这台机器从哪个版本升上来的」，只能在启动时
// 落一次盘（首见版本 + 最近版本），下次启动对比上一次记录的版本。
// 这份状态**纯本地**、不上报（除非管理员显式开启 update.report_url）。
//
// # 隐私边界（写进 docs/DIAGNOSTICS.md）
//
// install-version.json 只含版本与时间；instance-id 是 16 字节随机数，
// 与站点、账号、内容都没有映射关系——删掉文件即重置。两者都不含任何
// 可识别信息，故随诊断包/匿名上报一起带出是安全的。
//
// # 失败姿态
//
// 全部静默降级：数据目录不可写时 upgrade_from 为空、instance_id 退化为
// 进程内随机值（不持久化）。诊断端点不能因为「写不了文件」而整体失败。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oekit/core/version"
)

const (
	installVersionFile = "install-version.json"
	instanceIDFile     = "instance-id"
)

// installState 安装版本记录（install-version.json 的形态）。
type installState struct {
	// Version 最近一次启动的构建版本。
	Version string `json:"version"`
	// FirstSeen 首次见到该安装的时间（保留最早值）。
	FirstSeen time.Time `json:"first_seen"`
	// LastSeen 最近一次启动时间。
	LastSeen time.Time `json:"last_seen"`
}

var installMu sync.Mutex

// recordInstallVersion 记录本次启动版本，返回上次记录的版本（升级跨度）。
//
// 语义：
//   - 文件不存在 ⇒ upgradeFrom 为空（全新安装，不是升级）；
//   - 文件存在且版本不同 ⇒ upgradeFrom = 旧版本；
//   - 文件存在且版本相同 ⇒ upgradeFrom 为空（同版本重启）；
//   - 任何 IO 错误 ⇒ upgradeFrom 为空，且不阻塞启动。
func recordInstallVersion(dataPath, current string) string {
	if strings.TrimSpace(dataPath) == "" || strings.TrimSpace(current) == "" {
		return ""
	}
	installMu.Lock()
	defer installMu.Unlock()
	path := filepath.Join(dataPath, installVersionFile)
	now := time.Now().UTC()
	prev, err := readInstallState(path)
	upgradeFrom := ""
	if err == nil && prev.Version != "" && prev.Version != current {
		upgradeFrom = prev.Version
	}
	next := installState{Version: current, FirstSeen: now, LastSeen: now}
	if err == nil && !prev.FirstSeen.IsZero() {
		next.FirstSeen = prev.FirstSeen
	}
	// 写失败不影响任何功能：下次启动重新写即可。
	_ = writeInstallState(path, next)
	return upgradeFrom
}

func readInstallState(path string) (installState, error) {
	var st installState
	raw, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, err
	}
	return st, nil
}

func writeInstallState(path string, st installState) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// instanceID 返回安装实例的随机标识（hub#385 匿名上报用）。
//
// 每次调用都尽量持久化到数据目录；写不进去时返回进程内随机值——
// 该值只用于「同一实例多次上报」的去重，不是身份。
func instanceID(dataPath string) string {
	if strings.TrimSpace(dataPath) == "" {
		return randomID()
	}
	installMu.Lock()
	defer installMu.Unlock()
	path := filepath.Join(dataPath, instanceIDFile)
	if raw, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id
		}
	}
	id := randomID()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(id), 0o600)
	return id
}

// randomID 16 字节随机数的 hex（32 字符）。
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 熵源异常时退化为时间戳派生的占位值——它只影响去重粒度，
		// 不承载任何安全语义（不是密钥、不是会话 ID）。
		return "t" + hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// 进程内单例：诊断与更新检查共用同一份「安装指纹」，避免重复读写文件。
// ---------------------------------------------------------------------------

var (
	installOnce        sync.Once
	installUpgradeFrom string
	installID          string
)

// upgradeFromVersion 返回本次进程启动时的升级跨度（上一次运行的版本）。
//
// 只在第一次调用时对比/落盘：诊断端点会被反复读取，不能每读一次都改写
// last_seen（那会把「最近启动时间」变成「最近诊断时间」）。
func upgradeFromVersion() string {
	installOnce.Do(initInstallFingerprint)
	return installUpgradeFrom
}

// instanceIDFor 返回安装实例随机标识（进程内缓存；文件删了也不影响本进程）。
func instanceIDFor() string {
	installOnce.Do(initInstallFingerprint)
	return installID
}

func initInstallFingerprint() {
	dataPath := ""
	if config != nil {
		dataPath = config.App.DataPath
	}
	installUpgradeFrom = recordInstallVersion(dataPath, version.Version)
	installID = instanceID(dataPath)
}
