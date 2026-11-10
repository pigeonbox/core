# 分页成功响应统一迁移计划（core#124，**只写计划，未执行**）

状态：计划稿（2026-11-11，随 core#124 的口径表+守卫一起落地）。
本批**零 wire 变更**；真正动线上契约的步骤在波次 2/3，执行前需按本表再走一轮评审。

## 目标

把三套分页成功响应方言（resp.PageData / respondLegacyPage / IDL 生成模型）
逐步统一到**形态 C：contracts IDL 生成模型**（items/total/page/page_size/
total_pages），旧端点前端先双读、后端后切，最终删掉形态 A/B 构造器。

口径表（形态定义、端点清单、选型指引）：pkg/resp/page_forms.go。
机械守卫（禁止第四种形态）：pkg/resp/page_form_guard_test.go。

## 现状（波次 0 = 本批，已落地）

| 形态 | 端点 | 信封 |
|---|---|---|
| A | GET /admin/users/filter | code=0,message,data:{list,total,page,page_size} |
| B | GET /admin/logs/transfer、GET /admin/files/filter | code=0,message:"success",data:{items,total,page,page_size}（无 trace_id） |
| C | GET /admin/files、/admin/users、/admin/activities、/admin/notifies、/api/v1/user/shares、/api/v1/notifies/mine、/user/files | code,message,data:{…IDL 模型…}（分页字段各模型略有差异） |

波次 0 交付：口径表登记 + 守卫 + 本计划。wire 一字未动。

## 波次

### 波次 1 —— 前端双读（frontend-core，无 wire 变更）

- 前端把分页数据读取收敛成一个适配函数：同一端点先按形态 C 读
  （data.items / data.total / data.page / data.page_size / data.total_pages），
  读不到再回退旧形态（data.list（A）/ data.items+无 total_pages（B））。
- 双读对老后端完全兼容（现状按旧分支命中）；发布 frontend-core，随下一趟列车出货。
- 此波次**只有前端仓改动**，core 零变更。

### 波次 2 —— 形态 A 端点切换（core，wire 变更）

- GET /admin/users/filter：响应从 resp.PageData 切到 contracts IDL 模型
  （admin.AdminUserList 或新建 UserFilterData，分页字段对齐
  items/total/page/page_size/total_pages）。
- 前端已双读 ⇒ 无感；观察一个版本后再进波次 3。
- 守卫同步：resp.Page 调用点条目删除、resp.PageData 标记弃用。

### 波次 3 —— 形态 B 端点切换（core，wire 变更）

- GET /admin/logs/transfer、GET /admin/files/filter：外层信封从手工 map 切到
  IDL 模型；**item 字段逐字段保形**（手工小写 snake_case 是前端契约，只动
  信封与分页字段名：items 保持 items，补 total_pages）。
- respondLegacyPage 构造器删除，守卫对应条目删除。

### 波次 4 —— 收尾

- 删除 resp.Page / resp.PageData（先弃用一个版本再删）。
- 守卫收紧：形态 A/B 登记条目清零后，登记表只剩形态 C + 豁免条目；
  口径表把 A/B 标为「已退役」。
- 前端删除旧分支回退逻辑（双读→单读）。

## 前端可见契约变更点（逐条）

波次 2（/admin/users/filter）：
- data.list → data.items；
- 新增 data.total_pages（及可选 has_next/has_prev）；
- message 由 "success"（resp.Success 的 errors.GetMessage 口径）保持 code=0 语义不变。

波次 3（/admin/logs/transfer、/admin/files/filter）：
- 响应新增 trace_id（旧手工 map 没有；前端不依赖，向后兼容）；
- 新增 data.total_pages；
- message 保持 "success"（IDL 模型可显式传同值，避免消费方文案 diff）；
- ⚠️ item 字段名**不变**（只动信封）。

均不改：HTTP 状态（200）、code=0 成功语义、分页归一化口径（page/page_size
的默认/上限/超限语义一字不改，见 internal/paging 口径表）。

## 回滚口径

- 波次 1：前端单独出货、与后端解耦 ⇒ 回滚 = 回退 frontend-core 版本。
- 波次 2/3：每波次一个独立 PR/一趟列车，双读层保证前端同时兼容新旧 ⇒
  **回滚只需 revert 后端提交**，前端不必跟着回滚；回滚后旧形态继续命中
  双读的旧分支。
- 波次 4 的删除动作必须在波次 2/3 稳定一个版本之后；删除类回滚 = revert，
  但期间前端已单读 ⇒ 回滚窗口要跟着前端版本一起评估。

## 观察项（不在本计划波次内，顺带记录）

- GET /notifies/active 只回 data:{items}（无 total/page）——不是分页形态，
  但它是一次列表查询，消费方无从翻页；若要补分页字段，走形态 C（IDL 改
  contracts/notify.thrift），别造第五种。
- 形态 C 各模型的分页字段不一致（有的带 total_pages/has_next/has_prev、
  UserFileList 甚至把分页拆进 pagination 子对象）⇒ 波次 2/3 落地时可在
  contracts IDL 顺带定义统一分页子结构 Pagination，新模型一律嵌它；
  既有模型不动（wire 保形）。
