// Package paging 集中 core 全仓的**分页口径**（默认页长 / 上限 / 超限语义）。
//
// 为什么要有这个包：本仓历史上**手搓了多套**分页归一化（同一概念、多个实现、
// 两种超限语义），分别散在 app/notify、app/admin、app/mcp、repo/db/dao、
// gen/handler ⇒ 前端 / MCP 消费方在同一个 page_size 超限时拿到**不同条数**
// （可观测差异）。收编到 oekit/pkg/pagination 的 Policy 后，口径变成**声明式**
// 的一行常量，且两种超限语义（截断 / 回落）都能表达。
//
// # 口径表（**唯一登记处**——新增域先在此登记，别再各写一份）
//
//	#  域                     默认  上限      超限语义      位置
//	1  dao 通用（8 个 List）   20    200      截断 Clamp    repo/db/dao/page.go
//	2  dao 用户分享（带筛选）  20    100      截断 Clamp    repo/db/dao/filecode_usershares.go
//	3  notify 公告 / 通知     20    100      回落 Fallback app/notify/notify.go
//	4  mcp 分享列表           20    100      截断 Clamp    app/mcp/tools.go
//	5  mcp 用户列表           20    不限上限  —             app/mcp/tools.go
//	6  admin 系统日志         50    500      截断 Clamp    app/admin/system.go
//	7  admin 操作审计         20    200      截断 Clamp    gen/handler/admin/localfiles_ops.go
//	8  gen admin 列表 ×2      20    不限上限  —             gen/handler/admin/admin_service.go
//	9  gen user 文件列表      20    100      回落 Fallback gen/handler/user/user_service.go
//	10 gen share 用户分享     20    不限上限  —             gen/handler/share/share_service.go
//
// # 1–10 **全部已收编**到本包的 Policy（#8 于 2026-10-29 收编：第 8–10 项此前
// 标注「生成物，未收编」⇒ 口径清单漏了 3 项，正是这套方言当初失控的原因）。
//
// #8 的原判断是「这些文件是 hz/thriftgo 产物 ⇒ 改产物必被覆盖且必红」⇒ 实测
// **不成立**（2026-10-29 沙箱实证）：① `hz update` 对既有 handler 文件只**追加**
// 新方法的桩，**不重写**既有函数体（手写段落原样存活）；② CI 的 Gen drift check
// 对 `gen/handler` **只对账文件增删**（`gen/router` 才逐字节比对）⇒ 改既有
// handler 的内容不会让 CI 红。⇒ 与 `gen/handler/admin/localfiles_ops.go`
// （同为生成物目录下的手写文件、早已 import 本包）同款处置。
//
// ⚠️ 每一套口径都是**既有契约**（页长决定响应条数，消费方可见）⇒ 收编只改
// 「口径怎么表达」，**口径本身一字不改**：迁移前后每个域的返回条数必须逐条
// 一致——由各域的「现状锁定」用例钉住（dao / notify / admin / mcp 各一条）。
//
// ⚠️ 「回落」与「截断」在**同一输入下产出不同条数**（pageSize=101 ⇒ notify 得
// 20、mcp 分享列表得 100）⇒ **不可顺手统一**：消费方按域各自适应。
//
// 为什么不用 pkg/pagination 的包级常量：那些是**库默认值**（20 / 500 / 截断），
// 不是本仓任一域的口径 ⇒ 直接用会把上限从 200 / 100 变成 500 ⇒ 响应条数变化。
//
// 这些变量是**口径声明**，不是可变状态——请勿在运行期改写。
package paging

import "github.com/oekit/pkg/pagination"

// DAOPolicy 是 dao 通用分页口径（默认 20、上限 200、超限**截断**）。
var DAOPolicy = pagination.Policy{
	DefaultPage:     1,
	DefaultPageSize: 20,
	MaxPageSize:     200,
	OverMax:         pagination.OverMaxClamp,
}

// UserSharePolicy 是 dao 用户分享列表口径（默认 20、上限 100、超限**截断**）。
var UserSharePolicy = pagination.Policy{
	DefaultPage:     1,
	DefaultPageSize: 20,
	MaxPageSize:     100,
	OverMax:         pagination.OverMaxClamp,
}

// NotifyPolicy 是 notify 域分页口径（默认 20、上限 100、超限**回落**到默认）。
//
// ⚠️ 本域是**回落**（超限 ⇒ 回退 20），不是截断 ⇒ 与 dao 域同输入不同条数。
var NotifyPolicy = pagination.Policy{
	DefaultPage:     1,
	DefaultPageSize: 20,
	MaxPageSize:     100,
	OverMax:         pagination.OverMaxFallback,
}

// MCPListSharesPolicy 是 MCP 分享列表口径（默认 20、上限 100、超限**截断**）。
var MCPListSharesPolicy = pagination.Policy{
	DefaultPage:     1,
	DefaultPageSize: 20,
	MaxPageSize:     100,
	OverMax:         pagination.OverMaxClamp,
}

// MCPListUsersPolicy 是 MCP 用户列表口径（默认 20、**不设上限**）。
//
// ⚠️ MaxPageSize < 0 = 不限上限（pkg/pagination 的 Policy 语义）。
// 下游 dao 仍会按自身口径再钳一次，但本层报告的 page_size 是这里的值
// （与实际返回条数可能不一致——既有行为，本次收编不改它）。
var MCPListUsersPolicy = pagination.Policy{
	DefaultPage:     1,
	DefaultPageSize: 20,
	MaxPageSize:     -1,
	OverMax:         pagination.OverMaxClamp,
}

// AdminSystemLogPolicy 是系统日志口径（默认 **50**、上限 **500**、超限截断）。
//
// ⚠️ 默认值与上限都不同于其它域 ⇒ 不可与别的域共用口径。
var AdminSystemLogPolicy = pagination.Policy{
	DefaultPage:     1,
	DefaultPageSize: 50,
	MaxPageSize:     500,
	OverMax:         pagination.OverMaxClamp,
}

// AdminActivitiesPolicy 是管理操作审计口径（默认 20、上限 200、超限截断）。
var AdminActivitiesPolicy = pagination.Policy{
	DefaultPage:     1,
	DefaultPageSize: 20,
	MaxPageSize:     200,
	OverMax:         pagination.OverMaxClamp,
}

// ─────────────────── 以下 4 处：代码生成物（gen/handler）内的口径 ───────────────────
//
// #8 收编（2026-10-29）：这 4 处归一化此前是**手写代码寄居在生成物文件里**
// （gen/handler/{admin,user,share}/*_service.go）⇒ 口径表第 8–10 项长期标注
// 「生成物，未收编」⇒ 口径清单有 3 项漏项（正是这套方言当初失控的原因）。
//
// #8 原 issue 的前提是「这些文件是 hz/thriftgo 产物 ⇒ 直接改必被覆盖且必红」
// ⇒ **实测不成立**（2026-10-29 沙箱实证，见 gen/handler 的收编说明）：
//
//	 1. `hz update` 对**既有** handler 文件只**追加**新方法的桩，**不重写**既有
//	    函数体 ⇒ 手写段落原样存活（沙箱里 pageSize=20 那几行分毫不差）；
//	 2. CI 的 Gen drift check（CHECK=1）对 `gen/handler` **只对账文件增删**
//	    （`gen/router` 才逐字节比对）⇒ 改既有 handler 的**内容**不会让 CI 红。
//
// ⇒ 与 `gen/handler/admin/localfiles_ops.go`（同为生成物目录下的手写文件、
// 早已 import 本包）**同款处置**：口径声明收进本包，生成物里只留一行调用。
//
// ⚠️ 纪律不变（与 #5 同款）：收编只改「口径怎么表达」，**口径本身一字不改** ——
// 迁移前后每个域的返回条数必须逐条一致（由 policy_test.go 的边界用例钉住）。
//
// ⚠️ 本组三口径**互不相同**且与其它域不同（默认 20、user 是「回落 100」、
// admin/share 不限上限）⇒ 不可顺手统一。

// GenAdminListPolicy 是 gen admin 列表口径（默认 20、**不设上限**）。
//
// 对应 gen/handler/admin/admin_service.go 的 AdminListFiles 与 AdminListUsers
// 两处（原写法：page ≤ 0 ⇒ 1、pageSize ≤ 0 ⇒ 20，**无上限**）。
var GenAdminListPolicy = pagination.Policy{
	DefaultPage:     1,
	DefaultPageSize: 20,
	MaxPageSize:     -1,
	OverMax:         pagination.OverMaxClamp,
}

// GenUserFilesPolicy 是 gen user 文件列表口径（默认 20、上限 100、超限**回落**）。
//
// ⚠️ 本口径是**回落**（超限 ⇒ 回退 20），不是截断 ⇒ 与 dao 域同输入不同条数。
// 对应 gen/handler/user/user_service.go 的 UserFiles 一处
// （原写法：page < 1 ⇒ 1、pageSize < 1 **或 > 100** ⇒ 20）。
var GenUserFilesPolicy = pagination.Policy{
	DefaultPage:     1,
	DefaultPageSize: 20,
	MaxPageSize:     100,
	OverMax:         pagination.OverMaxFallback,
}

// GenShareUserSharesPolicy 是 gen share 用户分享列表口径（默认 20、**不设上限**）。
//
// 对应 gen/handler/share/share_service.go 的 UserSharesList 一处
// （原写法：page < 1 ⇒ 1、pageSize < 1 ⇒ 20，**无上限**）。
//
// ⚠️ 归一化发生在**查询之后**（原实现先把原始 page/pageSize 传给 service，
// 归一化只用于算响应里的 page/page_size/total_pages）⇒ 收编时**保持这个次序**
// 不变，否则「查询条数」与「响应里报告的页长」会错位（响应条数随之变化）。
var GenShareUserSharesPolicy = pagination.Policy{
	DefaultPage:     1,
	DefaultPageSize: 20,
	MaxPageSize:     -1,
	OverMax:         pagination.OverMaxClamp,
}
