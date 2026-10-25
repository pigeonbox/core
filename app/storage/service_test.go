package storage

import (
	"context"
	"fmt"
	"testing"

	"github.com/pigeonbox/core/conf"
	corestorage "github.com/pigeonbox/core/storage"
	"github.com/stretchr/testify/require"
)

type fakeRuntime struct {
	reloads []*corestorage.StorageConfig
	fail    bool
}

func (f *fakeRuntime) Reload(cfg *corestorage.StorageConfig) error {
	if f.fail {
		return fmt.Errorf("reload boom")
	}
	f.reloads = append(f.reloads, cfg)
	return nil
}

func (f *fakeRuntime) EffectiveType() corestorage.StorageType { return corestorage.StorageTypeLocal }

type fakePersister struct {
	saved []*conf.StorageConfig
}

func (f *fakePersister) LoadRuntimeStorage(context.Context) *conf.StorageConfig { return nil }

func (f *fakePersister) SaveRuntimeStorage(_ context.Context, c *conf.StorageConfig) error {
	f.saved = append(f.saved, c)
	return nil
}

func newFlowTestService(t *testing.T) (*Service, *fakeRuntime, *fakePersister) {
	t.Helper()
	dataPath := t.TempDir()
	conf.SetGlobalConfig(&conf.AppConfiguration{
		Server: conf.ServerConfig{Host: "127.0.0.1", Port: 12345},
		Storage: conf.StorageConfig{
			Type:        "local",
			StoragePath: dataPath,
		},
	})
	rt := &fakeRuntime{}
	p := &fakePersister{}
	s := NewService()
	s.SetRuntime(rt)
	s.SetPersister(p)
	return s, rt, p
}

func TestSwitchStorageFlow(t *testing.T) {
	ctx := context.Background()
	s, rt, p := newFlowTestService(t)

	// local → local：Probe（可写测试）+ Reload + 持久化全链路
	require.NoError(t, s.SwitchStorage(ctx, "local"))
	require.Equal(t, "local", conf.GetGlobalConfig().Storage.Type)
	require.Len(t, rt.reloads, 1)
	require.Len(t, p.saved, 1)
	require.Equal(t, "local", p.saved[0].Type)

	// s3 配置不完整 → activate 前置失败，内存配置不落、不持久化
	err := s.SwitchStorage(ctx, "s3")
	require.Error(t, err)
	require.Equal(t, "local", conf.GetGlobalConfig().Storage.Type)
	require.Len(t, p.saved, 1)

	// 不支持的类型
	require.Error(t, s.SwitchStorage(ctx, "onedrive"))

	// StorageInfo 含实际生效后端
	info, err := s.GetStorageInfo(ctx)
	require.NoError(t, err)
	require.Equal(t, "local", info.Effective)
}

func TestSwitchStorageReloadFailureRollback(t *testing.T) {
	ctx := context.Background()
	s, rt, p := newFlowTestService(t)
	rt.fail = true

	require.Error(t, s.SwitchStorage(ctx, "local"))
	// Reload 失败 → 不提交内存、不持久化
	require.Len(t, p.saved, 0)
	require.Equal(t, "local", conf.GetGlobalConfig().Storage.Type)
}

func TestUpdateStorageConfigWebDAVValidation(t *testing.T) {
	ctx := context.Background()
	s, _, p := newFlowTestService(t)

	// 非法 scheme：activate 的 SSRF 校验拦截
	// （扁平形态：请求体即 conf.StorageConfig 同构——旧嵌套形态已删）
	req := &UpdateConfigRequest{Type: "webdav"}
	req.WebDAV = &conf.WebDAVConfig{
		Endpoint: "ftp://example.com/dav", Username: "u", Password: "p",
	}
	require.Error(t, s.UpdateStorageConfig(ctx, req))
	require.Len(t, p.saved, 0)

	// 合法 URL（公网形态，不实际连接会被 Probe 拦）——这里仅验证校验通过后
	// Probe 失败同样不落配置（沙箱无外网，Probe 必败，恰好覆盖回滚路径）
	req.WebDAV.Endpoint = "https://webdav.invalid-host-pb-test.example/dav"
	require.Error(t, s.UpdateStorageConfig(ctx, req))
	require.Equal(t, "local", conf.GetGlobalConfig().Storage.Type)
	require.Len(t, p.saved, 0)
}
