package conf

// 全局配置的 **copy-on-write 并发安全**回归（2026-10-31）。
//
// # 缺陷形态（修复前）
//
// 全局配置是**裸包级指针** `globalConfig *AppConfiguration`，而
// app/config 的「新段热应用」（applySystemConfigOverlayLocked）与
// app/storage 的在线切换（activate）会**原地改写**它的字段：
//
//	g.UI = *sc.UI              // 整段结构体赋值
//	g.Upload = *sc.UploadEx    // 整段结构体赋值（几十个字段）
//	g.Storage.Type = ...       // 分多次写同一个段
//
// 而 GetGlobalConfig() 有 **45 处**读取点分布在请求路径上、**无锁读**这些字段
// ⇒ **结构体字段的数据竞争**：读者可能看到**撕裂的字段组合**
// （如 Upload.UploadSize 已是新值、RequireLogin 还是旧值）⇒ 闸口按半新半旧的配置判定。
//
// 这不是"启动竞态窗口"式的理论风险：**管理员在管理台保存配置的那一刻**
// 全站配置被整段改写，同刻有请求在读 ⇒ **必然发生**。
//
// # 本文件钉住什么
//
//  1. **读者永不看到撕裂配置**：并发读 + 并发热更新下，读到的快照必须是
//     **自洽的**（要么全旧、要么全新），不能是半新半旧；
//  2. 热更新后**新读者**必须看到新值（copy-on-write 没把更新吞掉）；
//  3. **负对照**：退回原地改写 ⇒ 「自洽性」断言必须红。

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// snapshotSelfConsistent 判断一个配置快照是否**自洽**。
//
// 判据：Upload.UploadSize 与 Upload.RequireLogin 必须同属"旧"或"新"两代。
// 原地改写时，这两字段分属不同赋值语句 ⇒ 并发读可能读到半新半旧的组合。
//
// 约定：UploadSize == 100 表示"旧代"，== 200 表示"新代"；
// 两代的 RequireLogin 取值不同（old=false / new=true）⇒ 可据此判自洽。
func snapshotSelfConsistent(c *AppConfiguration) bool {
	if c == nil {
		return false
	}
	switch c.Upload.UploadSize {
	case 100:
		return !c.Upload.RequireLogin
	case 200:
		return c.Upload.RequireLogin
	default:
		return false
	}
}

// TestGlobalConfig_CopyOnWrite_NeverTorn 并发热更新 + 并发读 ⇒ 永不撕裂。
func TestGlobalConfig_CopyOnWrite_NeverTorn(t *testing.T) {
	base := &AppConfiguration{}
	base.Upload.UploadSize = 100
	base.Upload.OpenUpload = true
	base.Upload.RequireLogin = false
	SetGlobalConfig(base)
	t.Cleanup(func() { SetGlobalConfig(&AppConfiguration{}) })

	const writers = 4
	const readers = 8
	const rounds = 200

	var wg sync.WaitGroup
	start := make(chan struct{})

	// 写侧：反复热应用「新代」配置
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				UpdateGlobalConfig(func(next *AppConfiguration) {
					next.Upload.UploadSize = 200
					next.Upload.OpenUpload = false
					next.Upload.RequireLogin = true
				})
			}
		}()
	}

	// 读侧：每读到一份快照就判自洽
	torn := make(chan string, 64)
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				c := Snapshot()
				if c == nil {
					torn <- "读到 nil 快照"
					return
				}
				if !snapshotSelfConsistent(c) {
					torn <- "读到撕裂的配置快照"
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()
	close(torn)

	for msg := range torn {
		t.Errorf("配置快照必须自洽（要么全旧、要么全新），却%s", msg)
	}
}

// TestGlobalConfig_UpdateVisibleToNewReaders 热更新后新读者必须看到新值。
func TestGlobalConfig_UpdateVisibleToNewReaders(t *testing.T) {
	base := &AppConfiguration{}
	base.Upload.UploadSize = 100
	SetGlobalConfig(base)
	t.Cleanup(func() { SetGlobalConfig(&AppConfiguration{}) })

	UpdateGlobalConfig(func(next *AppConfiguration) {
		next.Upload.UploadSize = 200
		next.Upload.OpenUpload = false
		next.Upload.RequireLogin = true
	})

	c := Snapshot()
	require.NotNil(t, c)
	assert.Equal(t, int64(200), c.Upload.UploadSize,
		"热更新后新读者必须看到新值（copy-on-write 没把更新吞掉）")
	assert.True(t, c.Upload.RequireLogin)
}

// TestGlobalConfig_UpdateDoesNotMutateOldSnapshot 更新不得改到旧快照上。
//
// 这是 copy-on-write 的**核心契约**：旧读者手里的快照必须保持不变。
func TestGlobalConfig_UpdateDoesNotMutateOldSnapshot(t *testing.T) {
	base := &AppConfiguration{}
	base.Upload.UploadSize = 100
	base.Upload.OpenUpload = true
	base.Upload.RequireLogin = false
	SetGlobalConfig(base)
	t.Cleanup(func() { SetGlobalConfig(&AppConfiguration{}) })

	old := Snapshot()
	require.NotNil(t, old)

	UpdateGlobalConfig(func(next *AppConfiguration) {
		next.Upload.UploadSize = 200
		next.Upload.OpenUpload = false
		next.Upload.RequireLogin = true
	})

	// 旧快照必须**原封不动**（否则旧读者会看到半新半旧）
	assert.Equal(t, int64(100), old.Upload.UploadSize,
		"旧快照不得被热更新改写（那正是撕裂读的来源）")
	assert.True(t, old.Upload.OpenUpload)
	assert.False(t, old.Upload.RequireLogin)
}
