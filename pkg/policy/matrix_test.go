package policy

import (
	"runtime"
	"strings"
	"testing"
)

// genRoot 定位仓库内 gen/ 目录（本包在 pkg/policy → 仓库根在 ../../）。
func genRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 失败")
	}
	// pkg/policy/xxx_test.go → 上两级 = 仓库根
	root := thisFile
	for i := 0; i < 3; i++ {
		if idx := strings.LastIndex(root, "/"); idx >= 0 {
			root = root[:idx]
		}
	}
	return root + "/gen"
}

// TestMatrixAllRoutesDeclared 正向：gen/router 全部路由已声明、声明无腐烂、鉴权一致。
// 新增公开路由未声明时本测试红（CI `go test -race ./...` 覆盖）。
func TestMatrixAllRoutesDeclared(t *testing.T) {
	inventory, err := ScanRoutes(genRoot(t))
	if err != nil {
		t.Fatalf("扫描 gen/router 失败: %v", err)
	}
	if len(inventory) < 100 {
		t.Fatalf("扫描结果异常（%d 条路由，应 >100）：解析器可能失效", len(inventory))
	}
	decls, err := LoadDeclarations()
	if err != nil {
		t.Fatalf("读取声明表失败: %v", err)
	}
	violations := Check(inventory, decls)
	for _, v := range violations {
		t.Errorf("策略矩阵违例: %s", v)
	}
}

// TestMatrixDeletedDeclarationTurnsRed 负对照（core#173 验收：删声明必红）：
// 从声明表内存中删除任意一条，Check 必须产出 STALE/MISSING 违例。
func TestMatrixDeletedDeclarationTurnsRed(t *testing.T) {
	inventory, err := ScanRoutes(genRoot(t))
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	decls, err := LoadDeclarations()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(decls) == 0 {
		t.Fatal("声明表为空")
	}
	trimmed := decls[:len(decls)-1]
	if violations := Check(inventory, trimmed); len(violations) == 0 {
		t.Fatal("负对照失败：删除声明后未产生违例（校验器失效）")
	}
}

// TestMatrixUnknownRouteTurnsRed 负对照：伪造一条未声明路由，Check 必须红。
func TestMatrixUnknownRouteTurnsRed(t *testing.T) {
	inventory, err := ScanRoutes(genRoot(t))
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	decls, err := LoadDeclarations()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	inventory = append(inventory, Route{
		Pkg: "share_anonymous", Method: "POST", Path: "/backdoor",
		Handler: "BackdoorExport", ChainAuth: "anonymous",
	})
	violations := Check(inventory, decls)
	found := false
	for _, v := range violations {
		if strings.Contains(v, "BackdoorExport") && strings.Contains(v, "MISSING-DECLARATION") {
			found = true
		}
	}
	if !found {
		t.Fatal("负对照失败：未声明的新路由未触发 MISSING-DECLARATION")
	}
}

// TestMatrixAuthCannotLoosen 声明不得比静态链更松：
// 链上有 AuthMiddleware 的路由声明为 anonymous 必须红；声明为 admin（更严）放行。
func TestMatrixAuthCannotLoosen(t *testing.T) {
	inventory := []Route{
		{Pkg: "user", Method: "GET", Path: "/profile", Handler: "GetProfile", ChainAuth: "user"},
	}
	decls := []Policy{
		{Pkg: "user", Handler: "GetProfile", Method: "GET", Path: "/profile", Auth: "anonymous", Bucket: "public-default", Ownership: "any"},
	}
	if violations := Check(inventory, decls); len(violations) == 0 {
		t.Fatal("声明 anonymous vs 静态 user 未触发 AUTH-MISMATCH")
	}
	// 更严声明放行
	decls[0].Auth = "admin"
	if violations := Check(inventory, decls); len(violations) != 0 {
		t.Fatalf("更严声明不应报违例: %v", violations)
	}
	// override 例外放行
	decls[0].Auth = "anonymous"
	decls[0].Override = "人工复核：白名单例外"
	if violations := Check(inventory, decls); len(violations) != 0 {
		t.Fatalf("override 例外不应报违例: %v", violations)
	}
}

// TestCore163PickupEntriesDeclared 验收：core#163 的取件面入口必须经矩阵受控。
// （share_anonymous 的取件/检索入口在声明表中且带公开频控桶。）
func TestCore163PickupEntriesDeclared(t *testing.T) {
	decls, err := LoadDeclarations()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	decl := map[string]bool{}
	for _, d := range decls {
		if d.Pkg == "share_anonymous" {
			decl[d.Handler] = true
		}
	}
	for _, h := range []string{"Download", "SearchByCode", "Retrieve", "GenerateCode"} {
		if !decl[h] {
			t.Errorf("取件面入口 %s 未进策略矩阵（core#163 回归防线失效）", h)
		}
	}
}
