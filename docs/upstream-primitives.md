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
