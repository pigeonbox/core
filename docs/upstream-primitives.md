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
