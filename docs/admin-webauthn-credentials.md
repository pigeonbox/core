# 管理台 Passkey 凭证管理（core#209）

管理员 Passkey（WebAuthn）的凭证列表与撤销端点。消费方为 frontend-core#118
（管理台安全页的凭证管理/撤销项）；登记/登录四端点见 core#160。

## 端点

两个端点都挂 `AdminMiddleware`（管理员 JWT / 会话 Cookie），只操作**当前
登录管理员自己**的凭证行。

### GET /admin/webauthn/credentials

成功响应（`data` 为数组，按登记时间正序）：

```json
{
  "code": 0,
  "message": "success",
  "data": [
    {
      "id": 12,
      "name": "办公 MacBook",
      "created_at": "2026-11-16T03:04:05Z",
      "last_used_at": "2026-11-16T08:09:10Z",
      "transports": ["internal", "hybrid"],
      "aaguid": "00112233445566778899aabbccddeeff"
    }
  ]
}
```

字段口径：

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | number | **服务端行 id**；撤销端点用它定位，credential_id 不出网 |
| `name` | string | 登记时用户起的别名 |
| `created_at` | string (RFC3339) | 登记时间 |
| `last_used_at` | string \| null | 最近一次断言验证成功时间；从未使用为 `null` |
| `transports` | string[] | 认证器传输方式（库登记时上报；无则 `[]`） |
| `aaguid` | string | 认证器 AAGUID（hex；全零 = 无 attestation） |

**不返回**：`credential_id`、公钥字节、`sign_count`、`user_id`、
`updated_at`/`deleted_at` 等服务端内部字段。`credential_id` 是登录
`allowCredentials` 的匹配键，出网只会扩大枚举面；撤销按不可猜测的行 id。

### DELETE /admin/webauthn/credentials/:id

`id` 必须是正整数（否则 400 `10001`）。归属校验下推到 SQL
（`WHERE user_id = ? AND id = ?`）：

- 自己的凭证存在 ⇒ 200，`message: "Passkey 已删除"`；
- 不存在 / **属于其他管理员** ⇒ 统一 404 `10004`
  （`Passkey 凭证不存在或已删除`），两者的响应完全一致，不泄露 id 是否存在；
- 重复删除 ⇒ 行保持不存在、无其他副作用，仍稳定返回 404
  （与 `DELETE /user/api-keys/:id` 的「不存在或已撤销」同口径）。

删除是**硬删**（物理删除行），不是软删：软删行仍占着 `credential_id` 的
唯一索引，同一张 passkey 撤销后重新登记会撞唯一约束；硬删同时保证所有
查询面（列表/计数/登录装配）彻底看不到该凭证。

## 语义边界

1. **删除后该 passkey 不能再参与登录。** 登录 `options` 的
   `allowCredentials` 不再包含它（无剩余凭证时按既有防枚举口径发同形诱饵）；
   `login/verify` 在装配用户凭证时查不到该行 ⇒ 无论是删除后新领的挑战，
   还是删除前已签好的断言，一律 `ErrWebAuthnInvalid`（400 `10001`）。
2. **口令 / TOTP / 恢复码路径不受影响。** 撤销只删 `webauthn_credentials`
   行，不碰 `users` 表的口令哈希与二验列；已开 TOTP 的管理员照常
   「口令 + 验证码」登录。
3. **删除最后一把 passkey ⇒ 自动回到未登记态。**
   `security.require_admin_webauthn`（`PB_SECURITY_REQUIRE_ADMIN_WEBAUTHN`）
   只约束「已登记 passkey 且未开 TOTP」的纯口令登录（判据
   `CountByUser > 0`）。删完最后一行后计数归零，该管理员与「从未登记过
   passkey」的存量账号同等待遇 ⇒ 纯口令重新放行，**不会把管理员锁死**。
   这是服务端行为，无需额外配置；撤销前的风险提示（「删掉最后一把后
   只能走口令/TOTP，请确认仍持有口令或二验」）由 UI 负责，后端不拦截。
4. **审计**：成功删除写运行日志（`webauthn credential deleted`，
   含 `user_id` 与凭证行 id），不记录公钥/credential_id。

## 测试与守卫

- `bootstrap/admin_webauthn_http_test.go`：匿名 401、list 字段形状精确
  断言（含内部字段不得出现）、只回自己、跨用户 404 且行保留、删除硬删、
  重复删除稳定 404、非法 id 400、删除后 `allowCredentials` 不再下发该凭证。
- `app/admin/webauthn_test.go`：服务层 list/delete 归属隔离、删除后
  真实断言被拒（含删除前已签好的断言）、`REQUIRE_ADMIN_WEBAUTHN` 在删除
  最后一把后自动放行、TOTP 路径不受影响。
- 两条手写路由已登记 `bootstrap/routes_classification_test.go` 豁免表
  （contracts 冻结，Pending 债务）与 `bootstrap/router_policy_matrix.txt`
  （`admin | global | custom`）。