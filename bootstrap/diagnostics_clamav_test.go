package bootstrap

// /admin/diagnostics 的 moderation.clamav 段断言（core#121）。
//
// 守卫点：
//  1. 未装配 ClamAV ⇒ enabled=false（不误报「已启用」）；
//  2. clamd 不可达 ⇒ degraded=true + last_ping.ok=false + 错误摘要
//     （issue 验收：停掉 clamd 诊断即显 degraded）——负对照锚点：
//     「Ping 恒绿」实现会让侧二转红；
//  3. fail-open 放行进窗口计数（degraded_count）；
//  4. **增段不动既有段**：既有顶层段键必须原样存在。
//
// fail-open 语义保持：本段只观测，不改变任何放行行为。

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	moderationApp "github.com/pigeonbox/core/app/moderation"
	"github.com/pigeonbox/core/conf"
)

// withClamAV 临时替换包级 clamavModerator（测试隔离用）。
func withClamAV(t *testing.T, m *moderationApp.ClamAVModerator) {
	t.Helper()
	old := clamavModerator
	clamavModerator = m
	t.Cleanup(func() { clamavModerator = old })
}

// fakeClamdPong 假 clamd：只回 PONG（诊断只探活）。返回 (addr, stop)。
func fakeClamdPong(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				r := bufio.NewReader(c)
				cmd, rerr := r.ReadString('\x00')
				if rerr != nil {
					return
				}
				if strings.HasPrefix(cmd, "zPING") {
					_, _ = c.Write([]byte("PONG\n"))
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// errOpener 恒失败的 ContentOpener（触发 open-failed fail-open 路径）。
type errOpener struct{}

func (errOpener) GetFileReader(context.Context, string) (io.ReadCloser, int64, error) {
	return nil, 0, assert.AnError
}

func TestDiagnosticsReport_ClamAVDisabledWhenNotWired(t *testing.T) {
	resetSetupAutoToken(t)
	withClamAV(t, nil)
	withTestConfig(t, &conf.AppConfiguration{
		App:        conf.AppConfig{DataPath: t.TempDir()},
		Deployment: conf.DeploymentConfig{Mode: conf.DeployModeStandalone},
	})

	report := diagnosticsReport(context.Background())
	assert.False(t, report.Moderation.ClamAV.Enabled, "未装配不得误报「已启用」")
	assert.Nil(t, report.Moderation.ClamAV.LastPing)
	assert.False(t, report.Moderation.ClamAV.Degraded)
	assert.Zero(t, report.Moderation.ClamAV.DegradedCount)
}

func TestDiagnosticsReport_ClamAVDegradedWhenClamdDown(t *testing.T) {
	resetSetupAutoToken(t)
	withClamAV(t, moderationApp.NewClamAVModerator("127.0.0.1:1", 1, 0, nil))
	withTestConfig(t, &conf.AppConfiguration{
		App:        conf.AppConfig{DataPath: t.TempDir()},
		Deployment: conf.DeploymentConfig{Mode: conf.DeployModeStandalone},
	})

	report := diagnosticsReport(context.Background())
	av := report.Moderation.ClamAV
	assert.True(t, av.Enabled)
	assert.Equal(t, "127.0.0.1:1", av.Addr)
	// 负对照锚点：Ping 恒绿（不真实探活）⇒ 下面两条同时转红。
	require.NotNil(t, av.LastPing, "启用时诊断读取前必须惰性探活")
	assert.False(t, av.LastPing.OK, "clamd 连不上时 last_ping.ok 必须为 false")
	assert.NotEmpty(t, av.LastPing.Error, "失败必须带错误摘要")
	assert.True(t, av.Degraded, "扫描器不可用必须显式 degraded（不得静默）")
}

// issue 验收：停掉 clamd ⇒ 诊断 30 秒内显示 degraded。
// 诊断读取前现场探活（无缓存）⇒ 停掉后**下一次读取**即显 degraded。
func TestDiagnosticsReport_ClamAVHealthyThenClamdStops(t *testing.T) {
	resetSetupAutoToken(t)
	addr, stop := fakeClamdPong(t)
	withClamAV(t, moderationApp.NewClamAVModerator(addr, 5, 0, nil))
	withTestConfig(t, &conf.AppConfiguration{
		App:        conf.AppConfig{DataPath: t.TempDir()},
		Deployment: conf.DeploymentConfig{Mode: conf.DeployModeStandalone},
	})

	first := diagnosticsReport(context.Background())
	require.NotNil(t, first.Moderation.ClamAV.LastPing)
	assert.True(t, first.Moderation.ClamAV.LastPing.OK)
	assert.False(t, first.Moderation.ClamAV.Degraded, "clamd 存活时不得报 degraded")

	stop() // 停掉 clamd
	second := diagnosticsReport(context.Background())
	require.NotNil(t, second.Moderation.ClamAV.LastPing)
	assert.False(t, second.Moderation.ClamAV.LastPing.OK, "停掉 clamd 后必须如实转红")
	assert.True(t, second.Moderation.ClamAV.Degraded, "停掉 clamd 后诊断必须显 degraded")
}

func TestDiagnosticsReport_ClamAVDegradedCountFromFailOpen(t *testing.T) {
	resetSetupAutoToken(t)
	m := moderationApp.NewClamAVModerator("127.0.0.1:1", 1, 1024, errOpener{})
	withClamAV(t, m)
	withTestConfig(t, &conf.AppConfiguration{
		App:        conf.AppConfig{DataPath: t.TempDir()},
		Deployment: conf.DeploymentConfig{Mode: conf.DeployModeStandalone},
	})

	for i := 0; i < 3; i++ {
		m.InspectFile(context.Background(), moderationApp.UploadMeta{
			StoragePath: "uploads/x.txt", FileName: "x.txt", Size: 1,
		})
	}
	report := diagnosticsReport(context.Background())
	av := report.Moderation.ClamAV
	assert.Equal(t, 3, av.ScansInWindow)
	assert.Equal(t, 3, av.DegradedCount, "degraded 计数 = 近 N 次扫描里 fail-open 放行次数")
	assert.Equal(t, 100, av.WindowSize)
}

// 增段不动既有段：既有顶层段键必须原样存在（core#121 只允许新增 moderation）。
func TestDiagnosticsReport_ExistingSectionsShapeUnchanged(t *testing.T) {
	resetSetupAutoToken(t)
	withClamAV(t, nil)
	withTestConfig(t, &conf.AppConfiguration{
		App:        conf.AppConfig{DataPath: t.TempDir()},
		Deployment: conf.DeploymentConfig{Mode: conf.DeployModeStandalone},
	})

	report := diagnosticsReport(context.Background())
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	var keys map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &keys))
	for _, want := range []string{
		"version", "deployment_mode", "server_time", "database", "storage",
		"redis", "site", "setup", "capabilities", "backup", "update",
	} {
		assert.Contains(t, keys, want, "既有段键不得因本次增段消失/改名")
	}
	assert.Contains(t, keys, "moderation", "新增段必须在 wire 上可见")
}
