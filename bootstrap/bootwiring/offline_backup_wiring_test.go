// Package bootwiring 是 bootstrap 装配层的**独立测试进程**（只有 _test.go）。
//
// 为什么单独成包：configApp.InitDefault / adminApp.InitDefault 是**进程级单例**
// （首次装配生效），bootstrap 包内的 handlers_public_test.go 已声明「本包唯一
// 数据源、首次装配」。同包再放一个 InitDefault 使用者会按测试执行顺序互相踩
// （谁先跑谁装配）。独立包 = 独立测试二进制 = 独立进程 ⇒ 单例归本包用例独占。
package bootwiring

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	configApp "github.com/pigeonbox/core/app/config"
	"github.com/pigeonbox/core/bootstrap"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1) // glebarez/sqlite :memory: 多连接各为独立库，钉死单连接防 flake
	require.NoError(t, gormDB.AutoMigrate(
		&model.SystemConfigRecord{}, &model.User{}, &model.FileCode{}, &model.FileCodeFile{},
		&model.AdminOperationLog{}, // SaveRuntimeStorage 的配置审计落这（缺表只告警，但免噪音）
	))
	return gormDB
}

// useGlobalConf 注入测试全局配置并在结束后还原（copy-on-write 语义见 conf 包）。
func useGlobalConf(t *testing.T, cfg *conf.AppConfiguration) {
	t.Helper()
	prev := conf.GetGlobalConfig()
	conf.SetGlobalConfig(cfg)
	t.Cleanup(func() { conf.SetGlobalConfig(prev) })
}

// TestOfflineBackup_RuntimeStorageEffective 守卫「离线备份/恢复必须吃 DB 持久化的
// runtime_storage（管理台在线切换的存储后端），不是只读 yaml」。
//
// 缺陷实录（2026-11-03，215 真机 S25d）：实例运行期把存储切到腾讯云 COS（配置在
// system_configs.runtime_storage），而 `pigeonbox backup export` 的离线装配只读
// yaml 的存储段（local）⇒ 对象按 local 目录找 ⇒ 全部 missing、object_count=0，
// 备份包只剩 DB 快照——灾备产物在最需要它的配置形态下静默缺对象。
//
// 负对照（本文件断言的牙齿）：把 NewOfflineBackupService 里的
// restoreRuntimeStorage() 调用去掉、storeBase 退回 &cfg.Storage ⇒
// 本用例两段断言都会转红（Phase A 生效配置不恢复、Phase B 对象找不到）。
//
// 两段合在一个用例里跑：configApp.InitDefault 是进程级单例（首次装配生效），
// 拆成多个用例会互相踩单例——见包注释。
func TestOfflineBackup_RuntimeStorageEffective(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	yamlDir := t.TempDir()    // yaml 意图的存储目录（对象**不在**这里）
	runtimeDir := t.TempDir() // 管理台切换后的存储目录（对象只在这里）

	// yaml 意图：local + yamlDir。离线装配的入参形态（server cmd 站在
	// InitConfig → SetGlobalConfig 之后传进来的就是这种 yaml 原文快照）。
	cfg := &conf.AppConfiguration{}
	cfg.Storage.Type = "local"
	cfg.Storage.StoragePath = yamlDir
	useGlobalConf(t, cfg)

	configApp.InitDefault(db)

	// ── Phase A：远端段全量恢复（不只 type——凭据段也得跟着走） ──
	require.NoError(t, configApp.Default().SaveRuntimeStorage(ctx, &conf.StorageConfig{
		Type: "cos",
		COS: &conf.CloudStorageConfig{
			Region:    "ap-test-1",
			Bucket:    "guard-bucket",
			AccessKey: "AKIDguard",
			SecretKey: "guardsecret",
		},
	}))

	svcA, err := bootstrap.NewOfflineBackupService(cfg, db)
	require.NoError(t, err)
	require.NotNil(t, svcA)
	effective := conf.Snapshot()
	require.Equal(t, "cos", effective.Storage.Type,
		"离线装配后生效存储段必须恢复 DB runtime_storage（不是 yaml 的 local）")
	require.NotNil(t, effective.Storage.COS, "凭据段必须随 type 一起恢复")
	require.Equal(t, "guard-bucket", effective.Storage.COS.Bucket)

	// ── Phase B：对象必须从 runtime_storage 的存储读（行为级对照） ──
	require.NoError(t, configApp.Default().SaveRuntimeStorage(ctx, &conf.StorageConfig{
		Type:        "local",
		StoragePath: runtimeDir,
	}))

	// 对象只存在于 runtimeDir；yamlDir 里**没有**——回归时（装配只读 yaml）
	// FileExists 找的是 yamlDir ⇒ 对象 missing、object_count=0。
	objRel := "uploads/2026/11/03/offline-backup-guard.txt"
	require.NoError(t, os.MkdirAll(filepath.Join(runtimeDir, filepath.Dir(objRel)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, objRel), []byte("guard"), 0o600))

	require.NoError(t, db.Create(&model.FileCode{
		Code:         "guard01",
		UUIDFileName: "offline-backup-guard.txt",
		FilePath:     objRel,
		Size:         5,
	}).Error)

	svcB, err := bootstrap.NewOfflineBackupService(cfg, db)
	require.NoError(t, err)

	var buf bytes.Buffer
	manifest, err := svcB.Export(ctx, &buf)
	require.NoError(t, err)
	require.Equal(t, int64(1), manifest.ObjectCount,
		"对象必须从 DB runtime_storage 的存储导出（不是 yaml 路径）")
	require.Empty(t, manifest.MissingObjects,
		"runtime 存储里存在的对象不得被记为 missing")
	require.Equal(t, "local", manifest.StorageType)
}
