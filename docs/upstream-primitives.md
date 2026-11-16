# 上游原语备案：已评估但未收编的条目

本文是 core#74 的落地载体。目的有两个，缺一不可：

1. **防误判「有原语不用」**——未来 review 时看到 oekit 里有现成原语而 core 手搓了一份，先查本文：可能已经评估过，且有明确的不采用理由。
2. **防「有形态硬凑」**——原语的形状与 core 的形态不匹配时，不要为了「用上外部包」而改造业务；等启用条件被满足再动。

纪律：新需求先查本文；某条的**启用条件被满足**时，先改本文的判定再写代码；条目被判为「收编」后从本文移除（历史在 issue #74 的对话里）。

## 1. `async.RunSafeCtx` / `RunSafeCtxRet`（concurrency v0.26，上游 #146）

- **形状**：在第三方 SDK 自己的 goroutine 边界内同步执行 + ctx 透传 + panic 收编为 `*PanicError` 返回给调用方。
- **启用条件**：接入会以自己的 goroutine 触发回调的第三方 SDK（ws 客户端事件回调等）。
- **当前判定：不动。** core 唯一的手写 recover 在 `app/share/zip.go` 的 `assembleZip`（`zip.go:80`），它处在 `concurrency.GoSafe` 边界内，自带「panic → `CloseWithError` → 重抛」补偿（语义与 RunSafeCtx 的「收编不杀循环」同向），且已配单测。**已核实仍成立（2026-11-08）**。

## 2. `poll.RunCursor`（concurrency v0.27，上游 #144）

- **形状**：单调游标长轮询批量拉取循环（游标只前进 / 处理完才推进 / 失败退避复用 retry / 单项 panic 不杀循环）。
- **启用条件**：接入 IM / 消息平台类游标拉取面。
- **当前判定：无消费者。** `grep -rln cursor app/` 为空——core 没有任何游标拉取循环。**已核实（2026-11-08）**。

## 3. `async/supervisor.Group[T]`（concurrency v0.27，上游 #143）

- **形状**：按键常驻循环集合与外部目标集持续对齐（新增起 / 消失停并排水 / 变更重启 / 死亡重启）。
- **启用条件**：「目标集来自外部存储、运行中持续对齐」的 worker 池。
- **当前判定：无消费者。** chunk 会话 janitor（`DeleteStaleSessions`）是**固定周期**清扫，不是「与外部目标集对齐」；联邦对端管理面目前只有一次性列举。`grep supervisor` 为空。**已核实（2026-11-08）**。

## 4. `ctxscope`（pkg v0.37.0，上游 #201）——**已收编（2026-11-08，core#74）**

原先的判定是「层不对：core 的请求上下文键在 hertz 层是 string 键」——**这条判定是错的**，已更正：

- hertz 侧确实是 string 键（`c.GetString("user_id")`，与 transport 常量同字符串），但**同时** core 把身份写进了 `context.Context`：`pkg/middleware/auth.go` 的 `withIdentity` 写入 5 个键、`app/mcp/server.go` 写入 1 个调用者键。这些正是 ctxscope 的目标形态。
- 原实现用手搓的非导出 `ctxKey` 枚举 + 空结构体 `callerCtxKey{}`：只防跨包键碰撞，**不钉值类型**（`ctx.Value(k).(uint)` 用错类型静默回零值 ⇒ 认证信息「消失」而不是报错）。
- 收编后：`ctxscope.Key[T]` 把「键 + 值类型」绑定（类型不匹配是编译错误），`From` 对绕过 `With` 的直写一律按不存在处理 ⇒ **身份只能经 `withIdentity` 单一入口写入**，这条保证由 `auth_ctxscope_test.go` 锁住。
- 零新增依赖（`oekit/pkg` 子包）。

## 5. `fanout.MapOption`（concurrency v0.27，BREAKING 更名）

- **形状**：`fanout.Option` → `MapOption` 更名。
- **当前判定：无影响。** core 零 `fanout` 使用（`grep fanout` 为空）。备案防未来按旧名误用后编译失败时翻不到原因。**已核实（2026-11-08）**。

## 6. 待回收：上游已修的下游补偿壳（登记制，core#85 起生效）

**形状**：因为上游原语缺口而打的下游绕过（补偿壳），上游修复发布后未拆。
风险：补偿壳带注释「因为上游有 bug」，但**没人跟踪上游修完的通知**⇒ 修复已进
当前钉版，壳还活着（永假分支、多余分配，且掩盖上游语义变化）。

**登记条目**（新发现一例登记一例，回收后划掉）：

- ~~`app/notify/notify.go doPush`：RawMessage 中转 + SyntaxError 判成功~~
  —— 已回收（2026-11-09，core#85）。上游缺口 oekit/pkg#158 修复已进当前钉版
  （pkg v0.38.0 的 `httpjson.decodeOK` 对 out==nil 跳过解码）。

**流程**：给补偿壳打 `docs/upstream-primitives.md` 同名登记（或直接在壳注释里
指向上游 issue 号）；上游修复 tag 落地、下游钉版抬过修复点后，按本节流程回收
并跑回归锁（2xx/非 2xx 两条，见 `app/notify/notify_dopush_test.go`）。

## 7. `gormx/crud` QuerySpec（gormx v0.11.7 同模块）——**试点中（2026-11-09，core#88）**

- **形状**：`crud.NewQuery()` 链式构建条件 → `Spec()` 物化为后端无关 QuerySpec →
  `gormrepo.ApplyQuery` 翻译为 gorm 链。三个正合语义钉在上游用例：
  `TestQuerySpec_FailClosed`（字段/操作符白名单外渲染恒假 1=0，绝不静默丢弃条件）、
  `TestQuerySpec_WhereGroup_Parenthesized`（Or 组带括号，不击穿 AND 链）、
  `TestQuerySpec_SnapshotIsolation`（Spec 快照隔离，构建器不是可变共享态）。
- **对 core 四类手拼风险的覆盖**：空条件退化全表（基线约束进 spec + fail-closed 用例）、
  占位符与参数漂移（条件即 (SQL 片段, args) 原子物）、用户输入字段注入面（字段白名单）、
  同链并发追加（快照隔离）。
- **试点三处（只换条件拼装，Repository 接口/返回类型/分页口径零变化）**：
  `dao.ListWithFilter`（管理台文件列表）、`dao.GetUserSharesWithFilter`（用户分享列表）、
  `dao.TransferLogRepository.List`（传输日志审计面）。落地见 `repo/db/dao/queriespec.go`
  与三个 spec 构建函数；EXPLAIN QUERY PLAN 与替换前逐条一致由
  `queriespec_pilot_test.go` 的 golden plan 锁（29 组合，含全字段叠加）。
- **扩大面之前先看**：① 条件词序是 sqlite 索引分析的前提，重排必须显式重录 golden；
  ② WhereGroup 的组内渲染是「Where 先于 Or」，`(A OR B) AND C` 形态要用嵌套组保序；
  ③ 多占位符 + ESCAPE 子句的 LIKE 片段走 `WhereRaw`（值仍占位符参数化）；
  ④ 排序/分页/Preload 不进 QuerySpec（本仓口径 paginate + core#8 Policy 不动）。
- **推广判定**：试点稳定一个列车周期后，再按「条件数最多 + 有用户输入参与」逐簇
  收编剩余手拼查询（42 文件不大爆炸）。

## 8. 2026-11-09 收编对账（当日 sweep 全量记录）

| 原语 | 上游 | core 落点 | PR |
|---|---|---|---|
| procx.Command（进程树终止：Setpgid 组击杀 + 跨组逃逸清扫 + windows Job Object） | pkg v0.39.0（#284→#304/#309） | preview.newProbeCmd 全量切换，D3 过渡约束退役 | #94 |
| procx.ChildEnv + WithOutputLimit（子进程环境清洗 + 输出封顶，「#294 三件套」收口） | pkg v0.41.0（#314） | preview/childenv.go 整表收编——自维护 previewEnvAllow 删除（allowlist 表只该有一份） | #100 |
| redisx ResilientLimiter（Redis 窗口+本地桶兜底组合原语，三态口径单源 + Source 可观测） | redisx v0.12.0（#118） | middleware ratelimit 阶段2；新增 `ratelimit_fallback_total{scope,source}` 降级观测 | #97 |
| kitx uidgen RandomHexE/From(io.Reader)（error 变体 + 随机源注入） | kitx v0.5.0（#87→#91） | 6 处手搓收编 + 3 处吞错上抛 + opendal genToken 漏项 | #93 |
| outbound（本仓内薄层，非上游） | — | pkg/outbound 统一注册表：三处外部客户端超时基准 + `outbound_requests_total` 观测 + 豁免表双向守卫 | #90 |
| gormx/crud QuerySpec（fail-closed + Or 组括号 + 快照隔离） | gormx v0.11.7 | 三处最复杂 DAO 试点（EXPLAIN golden 29 组合零差异） | #95 |

**核实记录**：core `pkg/memkv` 的 `_ = s.c.SetWithTTL(...)` 吞错核实为**无风险**——
底座 `oekit/pkg/cache.LocalCache.SetWithTTL` 恒返回 nil（v0.40.0 local.go:752-757，
写锁内直接落盘），error 仅接口形态（2026-11-09 核实）。

## 9. 2026-11-17 oekit 抬地板 sweep 评估（pkg v0.52.0 · core v0.15.4 · concurrency v0.38.1 · gormx v0.12.2 · kitx v0.10.1 · logx v0.11.0 · redisx v0.16.3 · network v0.11.2）

本节记录 sweep 当日**新出现**的上游原语与本仓判定。判定口径同前节：形状匹配 +
收益为正 + **不改语义**，三条同时成立才收编；任一不成立即登记不收编并写明启用条件。

### 9.1 自动兑现（同模块升级即吃到，本仓零改动）

| 上游 | 修复 | 对本仓的影响面 |
|---|---|---|
| redisx v0.16.1 | 坏条目归一为 miss 回源自愈（反序列化失败/空字节不再硬报错，MGet 单键损坏不整批失败） | 本仓直用 redisx（`pkg/middleware/ratelimit.go`、`pkg/gate/quota.go`、`pkg/middleware/rediscmd.go`）⇒ 直接吃到 |
| redisx v0.16.1 | `Refresh` 无 Source 入口 fail-fast（删除前返回 error，不再先清缓存后永不回源） | 同上 |
| redisx v0.16.1 | `NewClientFromConfig` 构造期校验地址（Host 非空、Port 1..65535，不再静默连本机回环） | 同上 |
| redisx v0.15.0 | `PeriodLimit`/`TokenLimiter` 补 `Close(ctx)` 幂等（探活 watchdog 停机口：Redis 永久不可达且 limiter 被丢弃时 goroutine 永不回收） | 同上（本仓 ratelimit 走 `redisx/ratelimit`） |
| redisx v0.15.0 | 降级桶缓存无界增长修复 + per-key singleflight（高基数 key 在长故障窗口不再沉淀全量条目常驻内存） | 同上 |
| redisx v0.15.0 | distlock 契约单源（`Locker` 改 `pkg/distlock.Locker` 类型别名，三哨兵改包级别名） | 本仓零使用 distlock ⇒ 无影响 |
| gormx v0.12.1 | `SplitStatements` 未闭合块注释 fail-closed、advisory lock 取消路径尽力解锁、分页溢出回显一致 | 本仓直用 gormx ⇒ 直接吃到 |
| gormx v0.11.15 | `GetByIDForUpdate` 主键 QuoteTo 参数化、scoper fail-closed、`Omit` 白名单收口 | 同上 |
| kitx v0.9.4 | snowflake 时钟早于 epoch 拒发、`uidgen` 随机源无进展快速失败（恒 0xFF 注入源不再死循环） | 本仓直用 kitx ⇒ 直接吃到 |
| concurrency v0.35.0 | `NewTokenBucket` 构造期校验 + `rps=0` 显式不限速档、`retry.Poll` interval≤0 构造期 panic、指数退避溢出边界修复 | 本仓直用 concurrency ⇒ 直接吃到 |
| concurrency v0.35.1 | `WaitGroupWrapper` 嵌套 `Wrap` 死锁修复（AB-BA 挂死且无 panic 无日志） | 本仓零使用该形态 ⇒ 无影响 |
| pkg v0.47.0–v0.52.0 | 29 单修复批（cron 契约 / config yaml 转义 / timex DST 非整小时回拨 / mathx 随机源统一等）、`timex` 周期端点不重叠、`errors.Newf`/`Wrapf` | 本仓直用 pkg ⇒ 直接吃到 |

### 9.2 已收编（上游补齐后本仓本就不重复实现，无需动）

- `convert.ResolveClientIP`（pkg v0.25.2→v0.26.0，#104/#106/#107 三单**全部 CLOSED**）：
  `pkg/middleware/clientip.go` 的「X-Real-IP 不采信 / 候选值合法性校验 / 候选值端口归一 /
  清单构造期解析」四处加固**上游已补齐**，本仓走 `convert.ResolveClientIPWithSet`。
  ⇒ 第 6 节补偿壳台账**无挂着未回收的条目**（本次 sweep 逐条复核确认）。

### 9.3 登记不收编（形状不匹配 / 语义冲突，写明启用条件）

| 原语 | 上游 | 不收编理由 | 启用条件 |
|---|---|---|---|
| `errors.Newf` / `errors.Wrapf` | pkg v0.52.0 | 形状匹配但**错误处理口径有差**：`Wrapf(nil, …)` 返回 nil，而 `fmt.Errorf("…: %w", nil)` 返回**非 nil** 错误（渲染为 `%!w(<nil>)`）⇒ 属「会改变行为」，不能机械批量替换；core 现存 591 处 / 111 文件，属独立大 diff | 走独立 PR：逐点替换 + nil 口径负对照 + 文案逐字断言 |
| `redisx.BruteForceGuard` | redisx v0.12.0+（v0.16.2 补 `Close(ctx)` 幂等 + `FailWithResult`） | 本仓 `pkg/middleware/lockout.go` 内存兜底**已收编** `concurrency/ratelimit.Lockout`，但 **Redis 侧仍手搓三步**（`IncrWithTTL` 计数 + `Set` 锁键 + `Del` 计数键）。上游形态是「ZSET 滑窗分布式锁 + 进程内降级」一体，且要求具体 `*redis.Client` ⇒ ①窗口语义变（滑窗 vs 本仓固定窗）②本仓 `redisLockCmds` 是**为注入内存 mock 而收窄的接口**，换上游会丢可测性 ③key 结构变（hash-tag ZSET vs 两把 key） | 若要为「计数—锁定」做原子化（当前三步非原子：计数与锁键分离，中间失败可致计数与锁定不一致），需先定「滑窗 vs 固定窗」口径并保留 mock 注入面 |
| `resumable`（分片上传/断点续传会话面：CAS 进度 + 断点双源对账 + 孤儿清扫） | pkg v0.44.0 | **语义冲突**：本仓铁律是「`upload_id` 由 `file_hash` 派生 ⇒ 重 init 会另起新会话使旧位图作废 ⇒ 续传必须对既有会话补片，不能重新 init」；上游 resumable 是「独立会话 + CAS 进度对账」范式，模型不同 | 上游的「孤儿清扫」能力本仓**已由 chunk 会话 janitor 覆盖**（`repo/db/dao/chunk.go` `DeleteStaleSessions`）⇒ 无新增需求则不收编 |
| `authz.SessionStore` + JWT `WithKeyID` | kitx v0.10.0 | 形状不匹配：本仓会话/refresh token 自持（`app/user` + `transport/http/handler/session_ops.go`），JWT 为自签对称（`pkg/auth/jwt.go` 直接依赖 `golang-jwt/jwt/v5`，非 kitx/jwt），与 kitx authz 的多实例注册/kick 编排不是同一问题域；`kid` 的目标场景是 JWKS 多钥轮换，本仓当前无此需求 | 引入 JWKS 多钥轮换，或会话存储形态与 kitx authz 对齐 |
| `transport/hertzbridge` | core v0.15.0 | 适配到**上游**的 transport 抽象；本仓自持 transport 且直接用 hertz（`transport/http/handler`），无「框架无关 transport 抽象」需求 ⇒ 收编它等于引入一层用不上的间接 | 本仓若要把 transport 抽成框架无关抽象（换 Hertz 之外的后端） |
| `scanrotation`（周期扫描墙钟预算 + 轮转游标记账件） | concurrency v0.37.0 | 本仓周期任务是**固定周期全量扫**（`bootstrap/tasks.go` 七件套 + `pkg/gate` 预算判定），无「分片轮转 + 防漏推钉子」的问题域 | 上「大规模分片扫描」（如 GC 全量扫按分片轮转）时 |
| `breaker.WithBreakerStateChangeHandler` | concurrency v0.38.0 | 形态不匹配：本仓走 redisx 的 `ResilientLimiter`（Redis 窗口 + 本地桶降级），不是 concurrency 的 breaker | 本仓引入 concurrency breaker 时 |
| gormx ILIKE 方言分派（MySQL 改写 LOWER） | gormx v0.12.1 | 本仓**刻意**用 `LIKE … ESCAPE '\'` + 自持 `EscapeLike`（`repo/db/dao/page.go`），跨库通用（ILIKE 在 MySQL 不存在、语义不可移植）；上游方言分派的前提是「用 ILIKE」 | 本仓改用 ILIKE 时（届时需处理 MySQL 缺 ILIKE） |
