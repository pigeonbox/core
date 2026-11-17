package anonymous

// redis_degrade_test.go —— 匿名取件面的 **Redis 运行期故障注入**（L6 故障注入）。
//
// 注入什么故障：
//   miniredis Close()（等价「Redis 运行期宕机」：监听端口消失 ⇒ 连接被拒），
//   随后 Restart()（同端口同客户端复活 ⇒ 故障恢复）。
//
// 断言什么（全部是**行为**）：
//   ① 宕机期**取件**仍可用：Redis 只是「取件码→分享码」的**加速缓存**，真相源是
//      file_codes.pickup_code 列（见 anonymous.go 的域注释「Redis 仅存映射 +
//      展示信息，真实状态查 DB」），且域内**已经有** DB 回源路径
//      （resolveShareCodeFromDB）⇒ 宕机期应当回落 DB，而不是整面不可用。
//   ② 宕机期**铸造**（写路径）仍可用：域契约明写「KV 写失败返回错误但调用方应
//      视为非致命（出码主流程不阻断）」（anonymous.go:510）⇒ 铸造不得因 Redis
//      宕机而整体失败，且铸出的码必须能从 DB 解析出来。
//   ③ 宕机期**建分享**不回滚：CreateAnonymousShare 的建行/铸造失败回滚会把用户
//      刚上传的分享整个删掉（anonymous.go:491-497）⇒ Redis 抖一下，用户的分享
//      就消失了。
//   ④ 恢复后**自动切回 Redis 路径**：新的映射键**真的写进 miniredis**
//      （anon:code:<取件码>），而不是永久停在降级态。
//   ⑤ 全程**不留半成品**：铸造失败时 DB 侧不得留下悬空的取件码（要么回滚、
//      要么能从 DB 解析），恢复后铸出的码必须两端一致（KV 有 + DB 有）。
//
// 怎么让它变红（负对照，详见交付说明的实跑记录）：
//   - 把 lookupShareCode 的「非 redis.Nil 错误」从「回落 DB」改回「直接返回错误」
//     ⇒ ①红（宕机期取件全挂）；
//   - 把 GenerateCode 的 KV 写失败分支从「保留 DB 取件码 + 返回码」改回
//     「ClearPickupCode + 返回错误」⇒ ②红、③红；
//   - 恢复后若客户端被永久降级闩锁（不再用 Redis）⇒ ④红（映射键不出现）。
//
// 用例隔离：miniredis + sqlite 内存库（与既有 anonymous_test.go 的 newTestService
// 同款构造），Redis 客户端用**快速失败**参数（否则故障期单条命令要等百毫秒级
// 的默认重试退避，判据会退化成时序竞态）。

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newRedisFaultService 起 miniredis + sqlite 内存库 + 匿名 service。
//
// 与既有 newTestService 的差别只有一处：Redis 客户端用快速失败参数
// （MaxRetries=-1 不在命令层重试；DialerRetries=1 让拨号失败也不重试——
// go-redis v9 的连接层默认重试 5 次、每次隔 100ms）。
func newRedisFaultService(t *testing.T) (*Service, *miniredis.Miniredis, *gorm.DB) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{
		Addr:          mr.Addr(),
		MaxRetries:    -1,
		DialerRetries: 1,
		DialTimeout:   10 * time.Millisecond,
		ReadTimeout:   2 * time.Second,
	})
	t.Cleanup(func() { _ = rdb.Close() })

	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{}, &model.Blob{}))
	// glebarez/sqlite 的 :memory: 每条连接是独立库 ⇒ 钉死单连接
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	return NewService(rdb, g), mr, g
}

// seedPickupShare 落一条「已完成上传」的分享（FilePath 非空 ⇒ 可取件）。
func seedPickupShare(t *testing.T, svc *Service, code string) {
	t.Helper()
	require.NoError(t, svc.fileCodeRepo.Create(context.Background(), &model.FileCode{
		Code:         code,
		FilePath:     "uploads/2026/11/18/" + code + ".bin",
		UUIDFileName: "demo.txt",
		Size:         128,
		ExpiredCount: -1,
	}))
}

// pickupMappingKey 取件码→分享码的 KV 映射键（与 anonymous.go 的 keyPickupCodeMapping 同口径）。
func pickupMappingKey(code string) string { return "anon:code:" + code }

// TestAnonymous_RedisOutage_RetrieveFallsBackToDB 宕机期取件仍可用。
//
// 这条是本文件的核心判据：Redis 在本域的定位是**加速缓存**（真相源 =
// file_codes.pickup_code 列），域内也已经写好了 DB 回源路径
// （resolveShareCodeFromDB）⇒ Redis 挂掉时取件必须回落 DB，而不是整面 500。
//
// 负对照：把 lookupShareCode 里「非 redis.Nil 错误」的早退改回「继续走 DB
// 回源」（即删掉 anonymous.go:216 的「非 redis.Nil 错误直接返回」那一段早退）
// ⇒ 本用例转绿；反之把 DB 回源分支删掉 ⇒ 本用例转红。
func TestAnonymous_RedisOutage_RetrieveFallsBackToDB(t *testing.T) {
	svc, mr, _ := newRedisFaultService(t)
	ctx := context.Background()

	seedPickupShare(t, svc, "OUTAGE1")

	// 健康期铸造取件码（此时 KV 映射 + DB pickup_code 双写）。
	code, err := svc.GenerateCode(ctx, CodeMeta{ShareCode: "OUTAGE1", FileName: "demo.txt", FileSize: 128},
		time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, code, codeLength)
	require.True(t, mr.Exists(pickupMappingKey(code)), "健康期映射键应写进 miniredis")

	// 宕机：Redis 运行期挂掉。
	mr.Close()

	// 期望：取件**仍然可用**（回落 DB 回源路径；真相源在 DB）。
	meta, err := svc.Retrieve(ctx, code, "", "")
	require.NoError(t, err, "Redis 宕机不得让匿名取件整面不可用（Redis 只是加速缓存，真相源在 DB）")
	require.NotNil(t, meta)
	assert.Equal(t, "OUTAGE1", meta.ShareCode, "应从 DB 回源解析出分享码")

	// Peek 走同一条 lookup 路径 ⇒ 同样必须回落 DB。
	meta2, fc, err := svc.Peek(ctx, code)
	require.NoError(t, err, "Peek 与 Retrieve 同一条解析路径，宕机期同样必须回落 DB")
	assert.Equal(t, "OUTAGE1", fc.Code)
	assert.Equal(t, "OUTAGE1", meta2.ShareCode)
}

// TestAnonymous_RedisOutage_MintStaysUsable 宕机期铸造（写路径）仍可用。
//
// 域契约（anonymous.go:510）：「KV 写失败返回错误但调用方应视为非致命
// （出码主流程不阻断）」⇒ 铸造不得因 Redis 宕机而失败；铸出的码必须能从
// DB 解析（否则用户拿到一个打不开的码）。
//
// 负对照：把 GenerateCode 的 KV 写失败分支改回「ClearPickupCode + 返回错误」
// （即恢复 anonymous.go:186-200 的原样）⇒ 本用例转红。
func TestAnonymous_RedisOutage_MintStaysUsable(t *testing.T) {
	svc, mr, _ := newRedisFaultService(t)
	ctx := context.Background()

	seedPickupShare(t, svc, "MINTFLT1")
	mr.Close()

	code, err := svc.GenerateCode(ctx, CodeMeta{ShareCode: "MINTFLT1", FileName: "demo.txt", FileSize: 128},
		time.Now().Add(time.Hour))
	require.NoError(t, err, "KV 写失败按域契约是非致命的（出码主流程不阻断）")
	require.Len(t, code, codeLength)

	// 铸出的码必须能从 DB 解析出来（否则用户拿到一个打不开的码）。
	fc, err := svc.fileCodeRepo.ResolveByCode(ctx, code, true)
	require.NoError(t, err, "铸出的取件码必须能从 DB（真相源）解析")
	assert.Equal(t, "MINTFLT1", fc.Code)
}

// TestAnonymous_RedisOutage_ShareCreationNotRolledBack 宕机期建分享不回滚。
//
// CreateAnonymousShare 先建 file_codes 行、再铸造取件码；铸造失败会把刚建的
// 行**物理删掉**（anonymous.go:491-497）⇒ Redis 抖一下，用户刚上传的分享
// 连同文件引用一起消失（文件还在盘上，分享没了）。
//
// 负对照：把 CreateAnonymousShare 的失败回滚去掉（只 return err）⇒ 本用例的
// 「分享行仍在」转红（行会留着但流程仍失败）；把 GenerateCode 的 KV 失败改回
// 「回滚 + 报错」⇒ 本用例整体转红。
func TestAnonymous_RedisOutage_ShareCreationNotRolledBack(t *testing.T) {
	svc, mr, g := newRedisFaultService(t)
	ctx := context.Background()
	mr.Close()

	// ⚠️ 必须带 ExpireAt（有限期分享）：永久分享（ExpireAt=nil）时
	// GenerateCode 走「零值 expireAt ⇒ 不写 KV」分支（anonymous.go:180），
	// 压根不碰 Redis ⇒ 测不到 KV 写失败 ⇒ 回滚路径。
	pickup, err := svc.CreateAnonymousShare(ctx, AnonymousShareParams{
		FilePath: "uploads/2026/11/18/new.bin",
		FileName: "new.txt",
		FileSize: 42,
		ExpireAt: ptrTimeA(time.Now().Add(time.Hour)),
	})
	require.NoError(t, err, "Redis 宕机不得让匿名分享整条链失败（KV 写失败是非致命的）")
	require.Len(t, pickup, codeLength)

	// 分享行必须还在（不得被失败回滚物理删掉）。
	var rows []model.FileCode
	require.NoError(t, g.Find(&rows).Error)
	require.Len(t, rows, 1, "失败回滚不得把用户刚建的分享删掉")
	assert.Equal(t, "uploads/2026/11/18/new.bin", rows[0].FilePath)
}

// TestAnonymous_RedisOutageThenRestart_ResumesRedisPath 恢复后自动切回
// Redis 路径：新的映射键**真的写进 miniredis**（不是永久停在降级态）。
//
// 编排：
//
//	① 健康期：铸码 ⇒ 映射键进 miniredis（Redis 路径证据）；
//	② 宕机期：铸码 ⇒ 无论成功还是失败，都**不留半成品**
//	   （失败 ⇒ DB 取件码已回滚；成功 ⇒ 该码能从 DB 解析出来）；
//	③ 恢复期（Restart，同端口同客户端）：铸码 ⇒ 新映射键进 miniredis。
func TestAnonymous_RedisOutageThenRestart_ResumesRedisPath(t *testing.T) {
	svc, mr, _ := newRedisFaultService(t)
	ctx := context.Background()

	seedPickupShare(t, svc, "RESUME01")

	// ① 健康期。
	healthy, err := svc.GenerateCode(ctx, CodeMeta{ShareCode: "RESUME01"}, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.True(t, mr.Exists(pickupMappingKey(healthy)), "健康期映射键应写进 miniredis")

	// ② 宕机期：铸码 ⇒ 不留半成品。
	mr.Close()
	outageCode, outageErr := svc.GenerateCode(ctx, CodeMeta{ShareCode: "RESUME01"}, time.Now().Add(time.Hour))
	if outageErr != nil {
		// 失败路径：DB 侧的取件码必须已回滚（不得留下悬空的取件码）。
		fc, gerr := svc.fileCodeRepo.GetByCode(ctx, "RESUME01")
		require.NoError(t, gerr)
		assert.Nil(t, fc.PickupCode, "铸造失败不得在 DB 留下悬空取件码（半成品）")
	} else {
		// 成功路径（降级铸造）：铸出的码必须能从 DB 解析（否则是打不开的码）。
		fc, gerr := svc.fileCodeRepo.ResolveByCode(ctx, outageCode, true)
		require.NoError(t, gerr, "降级铸造出的取件码必须能从 DB 解析")
		assert.Equal(t, "RESUME01", fc.Code)
	}

	// ③ 恢复期：同端口同客户端复活 ⇒ 自动切回 Redis 路径。
	require.NoError(t, mr.Restart())
	resumed, err := svc.GenerateCode(ctx, CodeMeta{ShareCode: "RESUME01"}, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.True(t, mr.Exists(pickupMappingKey(resumed)),
		"恢复后新映射键必须真的写进 miniredis（自动切回 Redis 路径）")

	// 恢复后取件照常（两端一致：KV 有 + DB 有）。
	fc, err := svc.fileCodeRepo.GetByCode(ctx, "RESUME01")
	require.NoError(t, err)
	require.NotNil(t, fc.PickupCode)
	assert.Equal(t, resumed, *fc.PickupCode, "恢复后 KV 映射与 DB 取件码必须指向同一个码")

	meta, err := svc.Retrieve(ctx, resumed, "", "")
	require.NoError(t, err)
	assert.Equal(t, "RESUME01", meta.ShareCode)
}
