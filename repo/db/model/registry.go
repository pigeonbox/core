package model

// 模型注册表 —— **全部领域模型的单一真相源**（2026-10-31 起）。
//
// # 为什么要有这个清单
//
// 此前「有哪些表」这件事**没有单一真相源**：模型定义散在各个文件里，而真正决定
// 建表范围的是 `repo/db/database.go` 里 `autoMigrate()` 那份**手写清单**。
// ⇒ 两处会漂移，而漂移的后果是**静默**的：
//
//	· 漏登记 ⇒ 升级路径的老库上那张表**永远缺失** ⇒ 该域整体 404/500
//	  （v0.11.1 实录：漏登 FilePreview ⇒ 所有升级库预览域整体 404）；
//	· 多登记 ⇒ 建出一张没人用的表（危害小，但仍是噪音）。
//
// 而「换库」（SQLite → PG/MySQL）把这件事从「顺手登记一下」升级成**硬需求**：
// 逻辑导入必须**逐表**搬运数据 ⇒ 漏一张表 = 那一域数据静默丢失。
//
// ⇒ 本文件把清单收敛到**一处**，并由两条路径共用：
//
//	1. `repo/db/database.go` 的 `autoMigrate()`（建表）；
//	2. `core/app/dbmigrate`（换库：逐表导出/导入 + 序列对齐）。
//
// # 机械守卫：漏登记即红
//
// `registry_guard_test.go` 用 **AST 扫描** `repo/db/model/*.go`，
// 找出所有「带表名的领域模型」并与本清单**逐一对账**：
//
//	· 模型文件里新增了一个模型但没登记 ⇒ 红；
//	· 本清单里登记了但模型不存在 ⇒ 红。
//
// ⇒ 「新增模型漏登记」从此不再是「上线后才发现」，而是**提交即红**。
//
// # 登记顺序：外键依赖在前
//
// 顺序即**导入顺序**（换库时必须先父表后子表，否则外键约束报错）：

type TableSpec struct {
	// Name 表名（与 GORM 实际落库名一致）。
	Name string
	// Model 模型实例（新实例即可，GORM 只用它做 schema 反射）。
	Model any
	// AutoIncrement 主键是否为自增。
	//
	// ⚠️ 这一位是**换库的地雷开关**：导入时显式写入 ID（保证主键不变 ⇒
	// 外键与「取件码/链接不变」的承诺成立）⇒ 目标库的**自增计数器不会前进**
	// （PG 的 `bigserial` 序列与 MySQL 的 `AUTO_INCREMENT` 都不跟进显式插入）
	// ⇒ 导入后必须显式把计数器重置到 `max(id)+1`，
	// 否则**下一次新建行立刻撞主键**。见 `app/dbmigrate` 的序列对齐步骤。
	AutoIncrement bool
}

// tables 是全部领域模型的登记清单（**唯一真相源**）。
//
// ⚠️ 改动本清单后，`registry_guard_test.go` 会自动核对；新增模型请**同步登记**。
var tables = []TableSpec{
	{Name: "users", Model: &User{}, AutoIncrement: true},
	{Name: "file_codes", Model: &FileCode{}, AutoIncrement: true},
	{Name: "file_code_files", Model: &FileCodeFile{}, AutoIncrement: true},
	{Name: "file_previews", Model: &FilePreview{}, AutoIncrement: true},
	{Name: "file_requests", Model: &FileRequest{}, AutoIncrement: true},
	{Name: "upload_chunks", Model: &UploadChunk{}, AutoIncrement: true},
	{Name: "transfer_logs", Model: &TransferLog{}, AutoIncrement: true},
	{Name: "admin_operation_logs", Model: &AdminOperationLog{}, AutoIncrement: true},
	{Name: "user_api_keys", Model: &UserAPIKey{}, AutoIncrement: true},
	{Name: "system_configs", Model: &SystemConfigRecord{}, AutoIncrement: true},
	{Name: "notifies", Model: &Notify{}, AutoIncrement: true},
	{Name: "pickup_charges", Model: &PickupCharge{}, AutoIncrement: true},
}

// Tables 返回全部领域模型的登记清单（**副本**，调用方改不动内部状态）。
func Tables() []TableSpec {
	out := make([]TableSpec, len(tables))
	copy(out, tables)
	return out
}

// TableNames 返回全部表名（按登记顺序）。
func TableNames() []string {
	out := make([]string, 0, len(tables))
	for _, t := range tables {
		out = append(out, t.Name)
	}
	return out
}

// AutoMigrateModels 返回可直接交给 `DB.AutoMigrate(...)` 的模型列表。
//
// 供 `repo/db/database.go` 的 `autoMigrate()` 使用 ⇒ 建表范围与换库搬运范围
// **必然一致**（同一份清单 ⇒ 不可能一个建了另一个没建）。
func AutoMigrateModels() []any {
	out := make([]any, 0, len(tables))
	for _, t := range tables {
		out = append(out, t.Model)
	}
	return out
}
