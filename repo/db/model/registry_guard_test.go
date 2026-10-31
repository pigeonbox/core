package model

// registry_guard_test.go —— **模型注册表的机械守卫**。
//
// # 为什么需要
//
// 注册表（`registry.go`）是「有哪些表」的**唯一真相源**，被两条路径共用：
// 建表（`repo/db/database.go` 的 `autoMigrate()`）与换库（`app/dbmigrate`）。
//
// ⇒ 它一旦与模型定义漂移，后果是**静默**的：
// 漏登记 ⇒ 老库上那张表永远缺失（v0.11.1 实录：漏登 FilePreview ⇒ 预览域整体 404）；
// 换库场景下更重 ⇒ 漏一张表 = 那一域数据在换库后**静默丢失**。
//
// ⇒ 本守卫用 **AST 扫描**找出模型文件里全部「领域模型」，与注册表**逐一对账**：
// 新增模型没登记 ⇒ 红；登记了但模型不存在 ⇒ 红。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm/schema"
)

// scannedModel 扫描出来的一个领域模型。
type scannedModel struct {
	// TypeName Go 类型名（如 "User"）。
	TypeName string
	// TableName 表名（从 TableName() 方法的字面量取；无该方法则按 GORM 复数化规则推）。
	TableName string
	// HasTableNameMethod 是否显式定义了 TableName()。
	HasTableNameMethod bool
}

// scanModels 用 AST 扫描模型目录，找出全部**领域模型**。
//
// 判定「领域模型」的标准（与 GORM 一致）：
//   - 是一个结构体；
//   - **不是** GORM 的 DTO（`*Query` / `*Update` / `*Stats` / `*Resp` 等后缀）——
//     DTO 不是表，不该登记；
//   - 不是纯辅助结构（如 `AdminOperationLogQuery`）。
//
// 识别表名：优先读 `TableName()` 方法的**字符串字面量**返回值；
// 没有该方法时按 GORM 的默认复数化规则推（`User` → `users`）。
func scanModels(t *testing.T) []scannedModel {
	t.Helper()

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	fset := token.NewFileSet()
	var out []scannedModel

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		require.NoErrorf(t, err, "解析 %s 失败", name)

		// 本文件里的 TableSpec 是注册表自身，不算模型。
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if ts.Name.Name == "TableSpec" {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				// DTO 排除：*Query / *Update / *Stats / *Resp / *Req / *Item …
				if isDTOTypeName(ts.Name.Name) {
					continue
				}
				// 领域模型必须真的映射一张表：内嵌 gorm.Model 或有 TableName 方法
				// 或含 gorm 标签字段 ⇒ 否则只是个普通结构体。
				if !looksLikeTable(st, ts.Name.Name) {
					continue
				}

				tbl, hasMethod := tableNameOf(f, ts.Name.Name)
				out = append(out, scannedModel{
					TypeName:          ts.Name.Name,
					TableName:         tbl,
					HasTableNameMethod: hasMethod,
				})
			}
		}
	}
	return out
}

// isDTOTypeName 判断是否 DTO（GORM 查询/更新/响应等非表结构）。
//
// ⚠️ **不要靠后缀猜**：本目录里 `FileRequest` 是**领域模型**（投递链接表），
// 而 `FileRequest`-like 的后缀（`Req`/`Request`/`Resp`…）既可能是 DTO、
// 也可能是表 ⇒ 纯后缀规则会**误杀领域模型**（实测：FileRequest 被当 DTO 丢掉
// ⇒ 守卫误报「file_requests 漂移」⇒ **守卫一旦说谎就没人信它**）。
//
// ⇒ 判据改为「**GORM 自己认不认它是表**」（见 looksLikeTable）：
// 用 GORM 的 schema 解析器判定，而不是复刻 GORM 的命名/类型规则。
func isDTOTypeName(name string) bool {
	// 明确的 DTO 命名（这些后缀在 GORM 生态里几乎专指非表结构）。
	for _, suffix := range []string{
		"Query", "Update", "Stats", "Resp", "Item", "Filter", "Option",
	} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// looksLikeTable 判断结构体是否像一张表。
func looksLikeTable(st *ast.StructType, typeName string) bool {
	for _, f := range st.Fields.List {
		// 内嵌 gorm.Model / gorm.DeletedAt 等
		if len(f.Names) == 0 {
			if sel, ok := f.Type.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "gorm" {
					return true
				}
			}
			continue
		}
		// 带 gorm 标签的字段 ⇒ 是表字段
		if f.Tag != nil {
			tag, err := strconv.Unquote(f.Tag.Value)
			if err == nil && strings.Contains(tag, "gorm:") {
				return true
			}
		}
	}
	return false
}

// tableNameOf 从文件里找该类型的 TableName() 方法，返回表名与是否显式定义。
func tableNameOf(f *ast.File, typeName string) (string, bool) {
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || fd.Recv.NumFields() != 1 {
			continue
		}
		recv, ok := fd.Recv.List[0].Type.(*ast.Ident)
		if !ok || recv.Name != typeName {
			continue
		}
		if fd.Name.Name != "TableName" {
			continue
		}
		for _, stmt := range fd.Body.List {
			ret, ok := stmt.(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				continue
			}
			lit, ok := ret.Results[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}
			return s, true
		}
	}
	return "", false
}

// gormDefaultTableName 用 **GORM 自己的命名策略**求默认表名。
//
// ⚠️ 不要自己复刻复数化规则：GORM 的 `NamingStrategy` 走 `toDBName`
// （含 `commonInitialisms` 缩写处理：APIKey → api_key）+ `inflection.Plural`
// ⇒ 自己手写规则必然对不上（实测：`FileCode` → `file_codes`（不是 file_codes？),
// `UserAPIKey` → `user_api_keys`）⇒ 守卫会误报「清单漂移」⇒ 守卫一旦说谎就没人信它。
func gormDefaultTableName(name string) string {
	ns := schema.NamingStrategy{}
	return ns.TableName(name)
}

// TestRegistryCoversAllModels —— **漏登记即红**。
func TestRegistryCoversAllModels(t *testing.T) {
	scanned := scanModels(t)
	require.NotEmpty(t, scanned, "未扫描到任何领域模型（扫描器本身可能坏了 ⇒ 先修扫描器）")

	registered := make(map[string]string, len(tables)) // 表名 → Go 类型名
	for _, t := range tables {
		registered[t.Name] = modelTypeName(t.Model)
	}

	// 1) 模型文件里的每个模型都必须在注册表里。
	for _, m := range scanned {
		want := m.TableName
		if want == "" {
			want = gormDefaultTableName(m.TypeName)
		}
		_, ok := registered[want]
		assert.Truef(t, ok,
			"模型 %s（表 %s）未登记进注册表 ⇒ 建表范围与换库搬运范围会漂移 ⇒ 该表数据在换库后静默丢失；请把它加进 repo/db/model/registry.go",
			m.TypeName, want)
	}

	// 2) 注册表里的每一项都必须对应一个真实存在的模型。
	scannedTables := make(map[string]bool, len(scanned))
	for _, m := range scanned {
		tbl := m.TableName
		if tbl == "" {
			tbl = gormDefaultTableName(m.TypeName)
		}
		scannedTables[tbl] = true
	}
	for _, spec := range tables {
		assert.Truef(t, scannedTables[spec.Name],
			"注册表登记了表 %s（模型 %s），但模型文件里找不到它 ⇒ 清单与现实漂移", spec.Name, modelTypeName(spec.Model))
	}

	// 3) 表名不得重复（重复会让换库「同一张表搬两次」）。
	seen := map[string]bool{}
	for _, spec := range tables {
		assert.Falsef(t, seen[spec.Name], "注册表里表 %s 重复登记", spec.Name)
		seen[spec.Name] = true
	}
}

// TestRegistryOrderRespectsForeignKeys —— 导入顺序必须父表在前。
//
// 换库是**逐表插入**（外键约束在 PG/MySQL 上是立即校验的）⇒ 顺序错了会直接报错。
// 父子关系（按模型定义）：
//
//	file_codes → file_code_files（FileCodeID）
//	file_codes → file_previews（FileCodeID）
func TestRegistryOrderRespectsForeignKeys(t *testing.T) {
	pos := make(map[string]int, len(tables))
	for i, t := range tables {
		pos[t.Name] = i
	}
	require.Contains(t, pos, "file_codes")
	require.Contains(t, pos, "file_code_files")
	require.Contains(t, pos, "file_previews")

	assert.Less(t, pos["file_codes"], pos["file_code_files"],
		"file_codes 必须排在 file_code_files 之前（外键 FileCodeID）")
	assert.Less(t, pos["file_codes"], pos["file_previews"],
		"file_codes 必须排在 file_previews 之前（外键 FileCodeID）")
}

// TestAutoMigrateModelsMatchesRegistry —— 建表清单与注册表必须同一份。
func TestAutoMigrateModelsMatchesRegistry(t *testing.T) {
	models := AutoMigrateModels()
	assert.Len(t, models, len(tables))
	assert.Equal(t, len(tables), len(TableNames()))
}

// modelTypeName 取模型实例的 Go 类型名。
func modelTypeName(m any) string {
	switch m.(type) {
	case *User:
		return "User"
	case *FileCode:
		return "FileCode"
	case *FileCodeFile:
		return "FileCodeFile"
	case *FilePreview:
		return "FilePreview"
	case *FileRequest:
		return "FileRequest"
	case *UploadChunk:
		return "UploadChunk"
	case *TransferLog:
		return "TransferLog"
	case *AdminOperationLog:
		return "AdminOperationLog"
	case *UserAPIKey:
		return "UserAPIKey"
	case *SystemConfigRecord:
		return "SystemConfigRecord"
	case *Notify:
		return "Notify"
	case *PickupCharge:
		return "PickupCharge"
	default:
		return "?"
	}
}
