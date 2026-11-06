package bootstrap

// 定时自动备份装配层的单测：目录解析（**绝对路径**纪律）与「未就绪时不 panic」。
//
// 导出逻辑本身的单测在 app/backup（含失败不删旧包、不留 .partial、并发拒绝等
// 负对照）；这里只测装配层容易出错的那部分——路径解析与降级。

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pigeonbox/core/conf"
)

// TestAutoBackupDir 目录解析：绝对路径原样、相对路径按**数据目录**解析、
// 缺省 `<datapath>/backups`。
//
// 为什么要专门测：相对路径按进程 CWD 解析是这套代码最容易犯的错（NAS 包 /
// 容器 / systemd 的 CWD 都不指向数据目录）⇒ 备份会写到莫名的地方或直接 EACCES。
func TestAutoBackupDir(t *testing.T) {
	prev := config
	defer func() { config = prev }()

	config = &conf.AppConfiguration{App: conf.AppConfig{DataPath: "/srv/pb/data"}}
	assert.Equal(t, "/srv/pb/data/backups", autoBackupDir(), "缺省 = 数据目录下的 backups")

	config = &conf.AppConfiguration{
		App:    conf.AppConfig{DataPath: "/srv/pb/data"},
		Backup: conf.BackupConfig{Dir: "/mnt/big/pb-backups"},
	}
	assert.Equal(t, "/mnt/big/pb-backups", autoBackupDir(), "绝对路径原样采用（可放独立大盘）")

	config = &conf.AppConfiguration{
		App:    conf.AppConfig{DataPath: "/srv/pb/data"},
		Backup: conf.BackupConfig{Dir: "snapshots"},
	}
	assert.Equal(t, filepath.Join("/srv/pb/data", "snapshots"), autoBackupDir(),
		"相对路径按数据目录解析，**不是**按进程 CWD")

	config = &conf.AppConfiguration{Backup: conf.BackupConfig{Dir: "bk"}}
	assert.Equal(t, filepath.Join("./data", "bk"), autoBackupDir(), "两边都缺省时仍给绝对化前的稳定形状")

	config = nil
	assert.Equal(t, filepath.Join("./data", "backups"), autoBackupDir(), "配置未就绪也不 panic")
}

// TestAutoBackupStatus_NotReady 依赖未就绪时只报状态、不 panic（诊断端点必须能答）。
func TestAutoBackupStatus_NotReady(t *testing.T) {
	prev := config
	defer func() { config = prev }()
	resetAutoBackupSingleton(t)

	config = &conf.AppConfiguration{
		App:    conf.AppConfig{DataPath: "/srv/pb/data"},
		Backup: conf.BackupConfig{Enabled: true, Keep: 7, At: "03:00"},
	}

	st := autoBackupStatus()
	// 目录与配置照样如实给出（运维要先知道「打算备份到哪」）。
	assert.Equal(t, "/srv/pb/data/backups", st.Dir)
	assert.True(t, st.Enabled)
	assert.Equal(t, 7, st.Keep)
	assert.Equal(t, "03:00", st.At)
	// 依赖缺失 ⇒ 落到 LastError，零份、未在跑。
	assert.NotEmpty(t, st.LastError, "依赖未就绪必须如实报出来")
	assert.Zero(t, st.Count)
	assert.False(t, st.Running)
}

// resetAutoBackupSingleton 清掉调度器单例（仅供测试）。
func resetAutoBackupSingleton(t *testing.T) {
	t.Helper()
	autoBackupMu.Lock()
	defer autoBackupMu.Unlock()
	autoBackupInited = false
	autoBackupSch = nil
	autoBackupErr = nil
}
