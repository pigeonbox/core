// FileCodeRepository.CountActive 不得复活（2026-10-26 审计 P2）。
//
// 缺陷：CountActive 是**死代码**（全仓无生产调用方），且口径是错的——
// `expired_at IS NULL OR expired_at > ? OR expired_count > 0` 把两个独立闸
// （时间过期 / 次数耗尽）OR 在一起 ⇒「时间已过期但次数没用尽」的 count 样式
// 分享被算成**活跃**，与 model.FileCode.IsExpired()（只判时间）结论相反
// ⇒ 一旦有人接上它，管理台统计虚高。
//
// ⇒ 删除该方法（同文件 CountToday 的同规格先例）。本守卫锁两件事：
//
//	① 方法不得复活（反射判定，避免「换个口径加回来」）；
//	② 全仓不得有它的调用方（源码树检索，避免「复活后立刻被接上」）。
//
// 断言必须能变红（负对照）：把 CountActive 加回 filecode.go 即失败。
package dao

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCountActiveRemoved_MethodMustNotExist CountActive 方法不得存在。
func TestCountActiveRemoved_MethodMustNotExist(t *testing.T) {
	_, ok := reflect.TypeOf(&FileCodeRepository{}).MethodByName("CountActive")
	assert.False(t, ok,
		"FileCodeRepository.CountActive 已删除（口径与 IsExpired 相反）：不得复活；"+
			"确有需要时按 IsExpired/IsCountExhausted 口径重建并写清「活跃」定义")
}

// TestCountActiveRemoved_NoCallers 全仓不得有 CountActive 的调用方。
func TestCountActiveRemoved_NoCallers(t *testing.T) {
	root := daoModuleRoot(t)
	// 只匹配方法调用形态（点号 + 方法名 + 左括号）；CountActiveByUser 是**另一个**
	// 方法（UserAPIKeyRepository），不被本守卫覆盖。
	// ⚠️ 模式串**拆开拼**（否则本守卫文件自身的注释/源码会命中自己的模式）。
	pat := regexp.MustCompile(`\.` + "CountActive" + `\(`)
	var hits []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "bin", "data":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if pat.Match(b) {
			rel, _ := filepath.Rel(root, path)
			hits = append(hits, rel)
		}
		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, hits,
		"CountActive 已删除（口径错误），不得有调用方：%v", hits)
}

// daoModuleRoot 从测试包目录向上定位模块根（含 go.mod 的目录）。
func daoModuleRoot(t *testing.T) string {
	t.Helper()
	root, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, statErr := os.Stat(filepath.Join(root, "go.mod")); statErr == nil {
			return root
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("向上未找到 go.mod，无法定位模块根")
		}
		root = parent
	}
}
