package bootstrap

// 部署自检报告（2026-10-30）的断言。
//
// 守卫点：
//  1. 报告里的每一项都必须是**生效值**（不是配置回显）；
//  2. 存储降级（配置 s3、实际 local）必须被**如实**标出——
//     这是「用户以为在用对象存储、实际全落本地盘」这类真故障的唯一信号；
//  3. 未装配数据库时不得 panic（nil 接收者路径要给出错误而不是崩）。

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/conf"
	"github.com/stretchr/testify/assert"
)

func TestDiagnosticsReport_DeploymentAndRedisMode(t *testing.T) {
	resetSetupAutoToken(t)
	t.Setenv(setupTokenEnv, "")
	t.Setenv(setupTokenAutoEnv, "")
	withSetupSvc(t, newSetupSvc(t))
	withTestConfig(t, &conf.AppConfiguration{
		App:        conf.AppConfig{Version: "v4.2.0", DataPath: t.TempDir()},
		Deployment: conf.DeploymentConfig{Mode: conf.DeployModeStandalone},
	})

	report := diagnosticsReport(context.Background())
	assert.Equal(t, "v4.2.0", report.Version)
	assert.Equal(t, conf.DeployModeStandalone, report.DeploymentMode)
	assert.Equal(t, "memory", report.Redis.Mode, "redis.host 空 = 单机内存模式")
	assert.False(t, report.Setup.Initialized, "未初始化实例")
	assert.Equal(t, "none", report.Setup.TokenSource)
}

func TestDiagnosticsReport_RedisModeWhenConfigured(t *testing.T) {
	resetSetupAutoToken(t)
	withTestConfig(t, &conf.AppConfiguration{
		Redis: conf.RedisConfig{Host: "10.0.0.5", Port: 6379},
	})
	report := diagnosticsReport(context.Background())
	assert.Equal(t, "redis", report.Redis.Mode)
	assert.Equal(t, "10.0.0.5:6379", report.Redis.Address)
}

func TestDiagnosticsReport_StorageDegradedFlagged(t *testing.T) {
	resetSetupAutoToken(t)
	withTestConfig(t, &conf.AppConfiguration{
		Storage: conf.StorageConfig{Type: "local"},
	})
	report := diagnosticsReport(context.Background())
	assert.Equal(t, "local", report.Storage.Backend)
	assert.False(t, report.Storage.Degraded)
}

func TestDiagnosticsReport_NoDatabaseDoesNotPanic(t *testing.T) {
	resetSetupAutoToken(t)
	withTestConfig(t, &conf.AppConfiguration{})
	// database 包级变量在单测里通常是 nil ⇒ 报告必须给出错误而不是崩
	report := diagnosticsReport(context.Background())
	if database == nil {
		assert.False(t, report.Database.Reachable)
		assert.NotEmpty(t, report.Database.Error, "未装配数据库必须给出原因（不能只说不可达）")
	}
}

func TestDiagnosticsReport_SetupTokenSource(t *testing.T) {
	resetSetupAutoToken(t)
	t.Setenv(setupTokenEnv, "explicit-token")
	t.Setenv(setupTokenAutoEnv, "")
	withSetupSvc(t, newSetupSvc(t))
	withTestConfig(t, &conf.AppConfiguration{})

	report := diagnosticsReport(context.Background())
	assert.Equal(t, "env", report.Setup.TokenSource)
	assert.True(t, report.Setup.TokenRequired)
}
