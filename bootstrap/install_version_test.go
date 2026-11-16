package bootstrap

// 安装指纹（hub#385）单测：升级跨度语义 + 实例 ID 持久化/重置。
//
// 断言是结构事实：返回的 upgrade_from 字符串、文件是否落盘、ID 是否稳定。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordInstallVersionSemantics(t *testing.T) {
	dir := t.TempDir()
	// 1) 全新安装：无前值。
	if got := recordInstallVersion(dir, "5.8.0"); got != "" {
		t.Fatalf("全新安装 upgrade_from 应为空, got %q", got)
	}
	// 2) 同版本重启：仍为空。
	if got := recordInstallVersion(dir, "5.8.0"); got != "" {
		t.Fatalf("同版本重启 upgrade_from 应为空, got %q", got)
	}
	// 3) 升级：返回上一个版本。
	if got := recordInstallVersion(dir, "5.9.0"); got != "5.8.0" {
		t.Fatalf("升级 upgrade_from 应为 5.8.0, got %q", got)
	}
	// 4) 文件形态：只含版本与时间。
	raw, err := os.ReadFile(filepath.Join(dir, installVersionFile))
	if err != nil {
		t.Fatalf("install-version.json 未写: %v", err)
	}
	for _, want := range []string{`"version"`, `"first_seen"`, `"last_seen"`, "5.9.0"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("文件缺少 %q: %s", want, raw)
		}
	}
	// 首见时间保留（升级不重置）。
	st, err := readInstallState(filepath.Join(dir, installVersionFile))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if st.FirstSeen.IsZero() || st.FirstSeen.After(st.LastSeen) {
		t.Fatalf("first_seen/last_seen 不合法: %+v", st)
	}
}

func TestRecordInstallVersionEmptyInputs(t *testing.T) {
	if got := recordInstallVersion("", "5.8.0"); got != "" {
		t.Fatalf("空 dataPath 应返回空, got %q", got)
	}
	if got := recordInstallVersion(t.TempDir(), ""); got != "" {
		t.Fatalf("空版本应返回空, got %q", got)
	}
}

func TestInstanceIDPersistsAndResets(t *testing.T) {
	dir := t.TempDir()
	id1 := instanceID(dir)
	if len(id1) != 32 {
		t.Fatalf("实例 ID 应为 32 hex, got %q", id1)
	}
	if id2 := instanceID(dir); id2 != id1 {
		t.Fatalf("实例 ID 应稳定: %q vs %q", id1, id2)
	}
	// 删文件 = 重置（issue 验收「随机实例 ID（可重置）」）。
	if err := os.Remove(filepath.Join(dir, instanceIDFile)); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if id3 := instanceID(dir); id3 == id1 {
		t.Fatalf("删除后应生成新 ID")
	}
	// 空数据目录返回进程内随机值（不落盘、不 panic）。
	if id := instanceID(""); len(id) != 32 {
		t.Fatalf("空 dataPath 应返回随机值, got %q", id)
	}
}
