// core#154 回归：ZipStream 先建流后扣次，chargePickup 失败早退必须关 rc。
//
// 缺陷：handler 在拿到 rc（io.Pipe + async.GoSafe 组装协程 + 已打开的存储
// 句柄）之后才 chargePickup，扣次失败直接 return 不 Close ⇒ 组装协程阻塞在
// 写管道直至 zipStreamTimeout=30min ⇒ 配额拦截反成资源放大器（次数耗尽者
// 每次重试都滞留一个协程 + 存储句柄）。
//
// 正判据：次数耗尽的多文件打包下载被 403 拒绝后，组装协程打开的存储句柄
// 必须及时归零（2s 内），不得滞留 30min。
// 负对照：撤掉守卫（rc 不 Close）⇒ 句柄滞留 ⇒ 本用例转红。
package share

import (
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	shareService "github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
)

// trackedStorage 包装真实存储、统计读句柄的开/关（组装协程滞留的可观测量）。
type trackedStorage struct {
	storage.StorageInterface
	mu     sync.Mutex
	open   int // 当前未关闭的读句柄
	opened int // 累计打开次数（单调：防「开了又关」的瞬态被轮询错过）
}

func (s *trackedStorage) GetFileReader(ctx context.Context, p string) (io.ReadCloser, int64, error) {
	rc, size, err := s.StorageInterface.GetFileReader(ctx, p)
	if err != nil {
		return rc, size, err
	}
	s.mu.Lock()
	s.open++
	s.opened++
	s.mu.Unlock()
	return &trackedReadCloser{rc: rc, s: s}, size, nil
}

func (s *trackedStorage) snapshot() (open, opened int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open, s.opened
}

type trackedReadCloser struct {
	rc   io.ReadCloser
	s    *trackedStorage
	once sync.Once
}

func (r *trackedReadCloser) Read(p []byte) (int, error) { return r.rc.Read(p) }

func (r *trackedReadCloser) Close() error {
	var err error
	r.once.Do(func() {
		err = r.rc.Close()
		r.s.mu.Lock()
		r.s.open--
		r.s.mu.Unlock()
	})
	return err
}

// newZipCloseTestEnv 内存 sqlite + 两个子文件的多文件分享环境 + 追踪存储。
func newZipCloseTestEnv(t *testing.T) (*gorm.DB, *trackedStorage) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{},
		&model.PickupCharge{}, &model.TransferLog{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1) // :memory: 多连接会各见独立库
	t.Cleanup(func() { _ = sqlDB.Close() })

	dataPath := t.TempDir()
	// a.bin 用**不可压缩**内容且远超 4KB：zip.NewWriter 在管道前垫了 bufio
	//（4KB，archive/zip/writer.go:50）——小文件/高可压缩内容会在缓冲里走完整个
	// entry、句柄被 assemble 正常关闭 ⇒泄漏场景下判据测不到滞留（#154 负对照
	// 实录：stall=0 却句柄全关）。随机 256KB ⇒ 压缩后仍 >>4KB ⇒ 组装协程必然
	// 在 CopyN 中途撞管道背压、句柄开在半路 ⇒ 只有读端被关闭（修复）才归零。
	big := make([]byte, 256<<10)
	_, err = rand.Read(big)
	require.NoError(t, err)
	files := map[string][]byte{"a.bin": big, "b.bin": []byte("body-b.bin")}
	for name, content := range files {
		p := filepath.Join(dataPath, "uploads", name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, content, 0o644))
	}

	oldCfg := conf.GetGlobalConfig()
	// 下载令牌关闭 ⇒ 扣次走 (分享码, 取件人 IP) 兜底会话键；零值其余全不限
	conf.SetGlobalConfig(&conf.AppConfiguration{})
	t.Cleanup(func() { conf.SetGlobalConfig(oldCfg) })

	inner := storage.NewStorageService(&storage.StorageConfig{
		Type:     storage.StorageTypeLocal,
		DataPath: dataPath,
	})
	tracked := &trackedStorage{StorageInterface: inner}
	SetShareService(shareService.NewService(g, "http://test.local", tracked))
	t.Cleanup(func() { SetShareService(nil) })
	return g, tracked
}

// TestZipStream_ChargeFailed_ClosesStreamPromptly 扣次失败路径的流清理判据。
func TestZipStream_ChargeFailed_ClosesStreamPromptly(t *testing.T) {
	g, tracked := newZipCloseTestEnv(t)
	ctx := context.Background()

	repo := dao.NewFileCodeRepository(g)
	fc := &model.FileCode{
		Code: "ZIPLEAK1", Text: "a.bin", FilePath: "uploads", UUIDFileName: "a.bin",
		Size: 256 << 10, ExpiredCount: 0, // 次数已耗尽 ⇒ chargePickup 必失败
	}
	require.NoError(t, repo.Create(ctx, fc))
	require.NotZero(t, fc.ID, "建分享后应回填主键")
	fileRepo := dao.NewFileCodeFileRepository(g)
	require.NoError(t, fileRepo.CreateBatch(ctx, []*model.FileCodeFile{
		{FileCodeID: fc.ID, FilePath: "uploads/a.bin", UUIDFileName: "a.bin", Size: 256 << 10},
		{FileCodeID: fc.ID, FilePath: "uploads/b.bin", UUIDFileName: "b.bin", Size: 10},
	}))

	// 扣次失败路径：次数耗尽的新取件会话 ⇒ 403 + 20004（请求本身被拒）
	code, body := performDownloadPath(t, "code=ZIPLEAK1")
	require.Equal(t, 403, code, "次数耗尽应在扣次处被拒：%s", body)
	require.Contains(t, body, "20004")

	// 组装协程确实在跑（至少打开过一个存储读句柄）——否则本用例测不到泄漏面
	require.Eventually(t, func() bool {
		_, opened := tracked.snapshot()
		return opened >= 1
	}, 2*time.Second, 5*time.Millisecond,
		"组装协程应打开存储读句柄（否则本用例测不到泄漏面）")

	// 关键判据：句柄必须及时归零——修复前组装协程滞留 zipStreamTimeout=30min
	require.Eventually(t, func() bool {
		open, _ := tracked.snapshot()
		return open == 0
	}, 2*time.Second, 5*time.Millisecond,
		"chargePickup 失败后存储句柄必须及时关闭（修复前组装协程滞留 zipStreamTimeout=30min）")

	// 顺带断言没有遗留打开的句柄（收尾快照，防 Eventually 时序错觉）
	open, opened := tracked.snapshot()
	require.Zero(t, open, "断言结束时不得有未关闭的存储句柄")
	require.GreaterOrEqual(t, opened, 1)
}
