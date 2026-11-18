# 目录策略「仅站内」（internal_only）语义与不变量（core#233）

状态：**已落地**（core PR 待合并，2026-11-18）。
真相源：本文件 —— core#233 issue 只记录进度与剩余面，语义以本文为准。

## 语义拍板（2026-11-18）

`share_mode` 的第三档 `internal_only` = **仅站内分享**。语义**两环都约束**（不是二选一）：

| 环 | 位置 | 行为 |
|---|---|---|
| 生成环 | 创建分享时 | **不铸造 6 位公网取件码**（只留 8 位分享码作站内入口） |
| 取件环 | 内容交付面 | **强制登录**：匿名一律拒（401 / CodeUnauthorized），已登录用户正常取件 |

创建面（本地导入命中该目录）**放行** internal_only：它不是「不许分享」，
而是「分享仅站内可取」。

为什么两环都约束：

- 只约束生成环 ⇒ 8 位分享码本身可传播，站外拿到仍能匿名取 ⇒ 「仅站内」名存实亡；
- 只约束取件环 ⇒ 6 位公网取件码照铸，码面本身就是「拿到就能匿名进」的凭证 ⇒
  站内/站外的边界在凭证层就漏了；
- 两环都约束 ⇒ 公网凭证不存在；且凭证即便泄露，交付面仍要登录 ⇒ 纵深防御。

## 取件面全集与拦截点（谁拦、拦什么）

| 面 | 身份能力 | internal_only 行为 | 拦截点 |
|---|---|---|---|
| `/share/select/`（取件查询） | OptionalIdentity | 匿名拒 / 登录放行 | `gen/handler/share.GetShare` → `GetFileByCodeFor` |
| `/share/download` | 无（令牌 = 已认证） | 无有效令牌 ⇒ 拒；有有效令牌 ⇒ 放行 | `authorizeDownload` 的 `!tokenValid` 分支 |
| `/share/metadata/:code` | 无 | **不拦**（与站点级 `download.require_login` 同口径：元数据面本来就不拦） | — |
| `/preview/:code` | 无（路由未挂身份中间件 ⇒ `UserID` 恒 nil） | 一律拒 | `app/preview.authorizePickup` |
| `/anonymous/retrieve` | 无 | 拒 | `app/anonymous.Retrieve` |
| `/anonymous/download/:code`、`/anonymous/search/:code` | 无 | 拒（两面共用 `Peek`） | `app/anonymous.Peek` |

**为什么必须拦匿名面**：匿名下载面会给「存在/未过期/未管控/无密码」的分享
**签发下载令牌**，令牌反手解锁 `/share/download` ⇒ 不拦 `Peek`/`Retrieve` 就是旁路。

**预览面的已知边界**：`/preview/:code` 路由未挂身份中间件 ⇒ `req.UserID` 恒 nil ⇒
站内已登录用户的预览也被拒（与全局 `download.require_login` 下 preview 的既有
行为一致）⇒ 预览面是「保守拒绝」，不是「按登录态放行」。

## 判定基准：建行时定型

`model.FileCode.InternalOnly`（`gorm:"default:false;not null" json:"-"`）在
**创建时**由本地导入命中的策略定型；取件热路径读**行上标记**，不查当前配置。

为什么不用「取件时按 `dir_root` 查当前配置」：

- **零 I/O**：取件热路径不做 `EvalSymlinks`（策略目录可能已被删/改名）；
- **零 fail-open**：查配置时若策略目录不可达 ⇒ 解析失败 ⇒ 条目被跳过 ⇒
  「仅站内」退化成公开；
- 与目录配额维度同款形态（都把目录维度的判定结果固化在行上）。

代价（明确边界）：策略改成 `internal_only` **不追溯存量**（与 `deny` 同款口径：
`deny` 只拦创建、不追溯存量）。

## 错误码

复用 `errcode.CodeUnauthorized`（10002），**不新增 contracts 码位** ⇒
不走 contracts 发版链。前端「登录后取件」的弹窗口径已存在
（全局 `download.require_login` 走同一码）。

## 取值词表的单一真相源

`conf.ShareModeAllow / ShareModeDeny / ShareModeInternalOnly` 三档封闭词表
（`conf/config.go`）。三处消费同一词表：

1. `app/share/dirpolicy.go` 运行期闸口（未知取值 fail-closed）；
2. `app/config/config.go` 管理台保存期校验（未知取值 400 + 10001，点名字段）；
3. `conf.DirectoryPolicyConfig.IsInternalOnly()`（创建面取标记）。

## 不变量（改动这条链前必读）

1. internal_only 的分享**不得**存在 6 位取件码（铸码处 `!req.InternalOnly` 是唯一开关）；
2. internal_only 的分享，匿名在**任何内容交付面**都拿不到内容（select/download/preview/匿名四面齐平）；
3. 下载令牌只由已登录用户在取件查询面取得 ⇒ 有效令牌 ⇒ 已认证；
4. 未知 `share_mode` 一律 fail-closed（保存期拒 + 运行期拒），不得静默放行；
5. 标记建行时定型；策略变更**不追溯存量**。

## 未覆盖（明确不在本单范围）

- **管理台 UI 选项**：frontend-core 的目录策略下拉仍是 allow/deny 两项 ⇒
  `internal_only` 只能经 API / yaml 设置（管理台 API 侧本 PR 已放行）。
  前端补选项属 frontend-core（marker 仓，须人工抬版本 + 打 tag）。
- **通用上传通道的目录归属**：直传 / 分片 / 预签名 / 多文件没有「来源目录」概念 ⇒
  不参与目录策略（含 `internal_only`）。
- **存量追溯**：策略变更不回收已入库的分享（与 `deny` 同款口径）。
- **元数据面**：匿名可见文件名（与 `download.require_login` 同口径，登录提示出现在取件时）。

## 代码落点

| 关注点 | 文件 |
|---|---|
| 词表 + 类型注释 | `conf/config.go` |
| 运行期策略闸口（创建面放行） | `app/share/dirpolicy.go` |
| 管理台保存期校验 | `app/config/config.go` |
| 标记定型（建行时） | `app/share/localimport.go` |
| 铸码（生成环） + 带身份取件入口 | `app/share/service.go` |
| 取件闸口原语 | `pkg/gate/gate.go` |
| 取件查询面 / 下载面 | `gen/handler/share/share_service.go` |
| 匿名域两面 | `app/anonymous/anonymous.go` |
| 预览面 | `app/preview/service.go` |
| 行上标记 | `repo/db/model/filecode.go` |

## 测试落点

- `app/share/internalonly_test.go`：铸码跳过 / allow 仍铸码 / 带身份取件放行与拒绝；
- `app/anonymous/internalonly_test.go`：`Peek` / `Retrieve` 拒绝 + 非仅站内放行；
- `pkg/gate/gate_internal_test.go`：闸口原语的匿名/登录两态；
- `conf/dirpolicy_test.go`：`IsInternalOnly` 的取值归一；
- `app/config/share_segment_test.go`：管理台保存期放行 `internal_only` + 未知档位仍拒。
