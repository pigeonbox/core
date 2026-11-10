// page_forms.go 分页成功响应口径表（core#124，**唯一登记处**）。
//
// # 为什么有这张表
//
// 错误信封已在 core#102/#104 统一到 resp.NewErrorWithMessage，但**成功的分页
// 响应**历史上有三套方言并存：resp.PageData / respondLegacyPage / IDL 生成模型。
// 前端按端点逐一适配，新增端点只能挑一种抄——每次都有 1/3 概率抄出第四种。
// 本表把三种形态登记成口径表（样式对齐 internal/paging 的口径表），并配套
// 机械守卫（page_form_guard_test.go）禁止第四种形态出现。
//
// ⚠️ 本表只登记**现状**，不改任何线上 wire 形状（core#124 硬边界：零 wire
// 变更）。逐步统一到形态 C 的迁移路径见 docs/pagination-envelope-migration.md
// （只写计划，未执行）。
//
// # 口径表（HTTP 200 成功响应的分页信封；新增分页端点只能三选一）
//
//	形态  信封结构（JSON）                                              构造器/类型（登记位置）
//	A     code=0,message,data:{list,total,page,page_size}              resp.PageData / resp.Page()（本包 response.go）
//	B     code=0,message:"success",data:{items,total,page,page_size}   respondLegacyPage（gen/handler/admin/admin_manage.go）
//	C     code,message,data:{…contracts IDL 生成模型…}                  contracts gen/* 模型（gen/handler 各处构造）
//
// **选型指引：新增分页端点只允许形态 C**（在 contracts IDL 定义 Req/Resp、用
// 生成模型构造）；形态 A/B 是历史兼容，**冻结**——守卫按调用点计数棘轮钉住，
// 不得新增调用点。
//
// # 形态 A —— resp.PageData（历史，冻结）
//
//	字段：data.list=条目数组；data.total=总条数；data.page；data.page_size。
//	端点：GET /admin/users/filter
//		（gen/handler/admin/admin_manage.go AdminListUsersFiltered）。
//	构造器：resp.Page(...)（→ resp.Success(c, PageData{...})）。
//
// # 形态 B —— respondLegacyPage（历史，冻结）
//
//	字段：data.items；data.total；data.page；data.page_size；
//		message 恒为 "success"；无 trace_id（手工 map，不走 resp.Response）。
//	端点：GET /admin/logs/transfer（AdminTransferLogs）、
//		GET /admin/files/filter（AdminListFilesFiltered）。
//	构造器：respondLegacyPage（gen/handler/admin/admin_manage.go，手工 map——
//	全仓唯一允许的分页手工 map，守卫按「文件+函数」豁免登记）。
//	⚠️ item 字段是手工小写 snake_case（前端逐字段契约），只许保形。
//
// # 形态 C —— IDL 生成模型（**新增端点唯一允许的形态**）
//
//	字段：data 由 contracts gen 模型序列化，分页字段按模型略有差异：
//
//		admin.AdminFileList / admin.AdminUserList / admin.AdminActivitiesData
//			items,total,page,page_size
//		sharemodel.UserSharesListData
//			items,total,page,page_size,total_pages,has_next,has_prev
//		notifymodel.ListData
//			items,total,page,page_size
//		notifymodel.MineData
//			items,total,unread,page,page_size,total_pages
//		usermodel.UserFileList
//			files,pagination{page,page_size,total,total_pages,has_next,has_prev}
//
//	端点：
//
//		GET /admin/files          （gen/handler/admin/admin_service.go AdminListFiles）
//		GET /admin/users          （gen/handler/admin/admin_service.go AdminListUsers）
//		GET /admin/activities     （gen/handler/admin/localfiles_ops.go AdminActivities）
//		GET /api/v1/user/shares   （gen/handler/share/share_service.go UserSharesList）
//		GET /admin/notifies       （gen/handler/notify/notify_service.go List
//			→ notifymodel.ListData）
//		GET /api/v1/notifies/mine （gen/handler/notify/notify_service.go Mine）
//		GET /user/files           （gen/handler/user/user_service.go UserFiles，
//			app/user/service.go GetUserFiles 构造 usermodel.UserFileList）
//
//	构造点在 gen/handler/* 与 app/user/service.go；字段真相源 = contracts IDL
//	（idl/*.thrift），新增端点在 IDL 里把分页字段对齐 items/total/page/
//	page_size/total_pages ⇒ 单一形态，前端只抄一种。
//
// # 非 HTTP 分页信封（同形载体，登记防漂移——**不是**第四种 wire 方言）
//
//	· MCP structuredContent：app/mcp/tools.go listSharesOut / listUsersOut
//		（MCP JSON-RPC 工具输出，不走 HTTP 响应信封，jsonschema 契约独立）。
//	· app 内部载体：app/notify/notify.go ListData / UserNotifyListData
//		（service 层返回值；wire 侧经 gen/handler/notify 的 toListData / Mine
//		重映射落形态 C；/notifies/active 只回 {items} 无分页字段——见迁移
//		计划的观察项）。
//
// 这两类载体也含 total+page 字段、机械上与三形态同形 ⇒ 一并进守卫登记表
// （白名单带理由、计数棘轮），防止有人顺手把它们当 wire 信封直接吐出去。
//
// # 机械守卫（page_form_guard_test.go，按**构造点/调用点**判定）
//
//	① 全仓含 total+page/page_size 的载体（结构体声明 / 字面量 / 手工 map）
//		必须在守卫登记表登记（形态+位置+条数；棘轮只许减，条目失效也判红）；
//	② 形态 A/B 构造器（resp.Page / respondLegacyPage）调用点计数冻结，
//		不得新增（core#8/#104 同款「按调用点判定 + 白名单棘轮」先例）；
//	③ 造出第四种形态 ⇒ 红（负对照：守卫文件里的合成第四形态用例）。
package resp
