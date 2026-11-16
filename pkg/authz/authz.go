// authz.go 授权判定的单一执行点（core#173 第五片：Authorization 收口）。
//
// ## 为什么有这个文件
//
// 授权三类判据此前散落各处，新入口照抄即可绕过，且没有任何测试会红：
//
//   - 角色判定：pkg/middleware 的 AdminMiddleware/AdminOrAdminKey、transport 的
//     IsAdmin/RequireAdmin/RequireRole/RequireAnyRole、routes_custom 的 MCP
//     Caller.Admin、app/admin 登录与 Passkey 四处「role==admin && status==active」、
//     app/oidc 半会话签发条件——「admin」字面量散在 8 个文件各写一份；
//   - 资源归属：share 删除/取件时间线各写一份「UserID == nil || *UserID != caller」、
//     chunk 会话三通道归属（IP/令牌/用户）里内联两份用户比对、presign object_key
//     归属内联第三份——nil 语义/参数顺序一旦抄错即成越权或误拒；
//   - Scope（API Key 作用域）已在 pkg/middleware/apikey.go 单点且被
//     authn_entry_guard 钉住，本包不重做。
//
// 2026-11-16 收口于此：
//
//   - 角色：IsAdminRole / IsActiveAdmin / RoleSatisfies / AnyRoleSatisfies；
//   - 归属：OwnedByUser（属主与调用方同一用户，nil 不匹配）/ OwnedByIP
//     （匿名属主 IP 匹配，空 IP 不参与）；
//   - **只判定不写响应**——各入口的 401/403/404 文案与状态码方言由调用方映射，
//     这是「行为零变化」的关键（跨用户 404 不泄露存在性等既有口径一字不动）。
//
// 守卫：bootstrap/authz_entry_guard_test.go —— 路由链上的角色口径必须登记到
// 执行点承载文件、源码出现裸角色/归属判定（Role != "admin" 等）⇒ 测试红。
package authz

// IsAdminRole 是否管理员角色（"admin" 字面量的唯一判据点）。
func IsAdminRole(role string) bool {
	return role == "admin"
}

// IsActiveAdmin 是否在职管理员（role=admin 且 status=active）。
// 管理台 Passkey 四个入口（登记/断言校验/登录目标/登录验证）逐字重复该组合，
// 降权或禁用任一条件都必须挡住。
func IsActiveAdmin(role, status string) bool {
	return role == "admin" && status == "active"
}

// RoleSatisfies 当前角色是否满足所需角色（RequireRole 中间件的判定核）。
func RoleSatisfies(current, required string) bool {
	return current == required
}

// AnyRoleSatisfies 当前角色是否满足任一所需角色（RequireAnyRole 中间件的判定核）。
func AnyRoleSatisfies(current string, allowed ...string) bool {
	for _, r := range allowed {
		if current == r {
			return true
		}
	}
	return false
}

// OwnedByUser 资源属主是否为当前调用方（归属判定核）。
//
// 任一为 nil 即不匹配：属主 nil = 匿名/无主资源，不归任何登录用户；
// 调用方 nil = 未登录。**判定只认用户 ID**——IP/令牌通道由各域的会话归属
// （app/chunk.OwnedByCaller 等）自行组合，本函数不替代它们的多通道语义。
func OwnedByUser(ownerUserID, callerUserID *uint) bool {
	return ownerUserID != nil && callerUserID != nil && *ownerUserID == *callerUserID
}

// OwnedByIP 匿名资源属主 IP 是否匹配调用方（匿名归属判定核）。
//
// 属主 IP 为空即不匹配：空串相等会让两个都取不到 IP 的调用方互相命中
// （把归属修复重新打穿，见 dao.FirstByUploadIDOwnedBy 注释）。
func OwnedByIP(ownerIP, callerIP string) bool {
	return ownerIP != "" && ownerIP == callerIP
}
