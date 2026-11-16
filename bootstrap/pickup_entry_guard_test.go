// 取件入口收口守卫（core#173 第二片：Pickup Policy 收口）。
//
// ## 为什么有这道门禁
//
// pkg/gate 是取件判定的单一执行点（PrecheckPickup = 枚举/防爆破锁定预检；
// CheckPickup = 过期/管控/密码判定），但**没有任何机制要求新入口调用它们**
// ——core#163 正是这样被绕过的：metadata/search 等公开入口既无预检也无记账，
// 枚举只剩 QPS 限流桶（50/s vs 锁定 10 次/300s，约 4500× 吞吐差）。
//
// ⇒ 本守卫把「每个交付面必须走收口点」变成机械判据：
//
//	① 真实注册路由里的 HTTP 交付面（由取件契约表 shareSurfaceContracts 承接，
//	   新面漏登记由 TestShareSurfaceContractCoverage 拦）必须在 pickupFaces
//	   登记收口点承载文件——新面未登记 ⇒ 红；
//	② router_policy_matrix.txt（#203 产物，只读引用）里三个取件前缀下的
//	   public 路由必须都在契约表内，且交付面必须在 pickupFaces 内——
//	   矩阵新增取件面未登记 ⇒ 红；
//	③ 承载文件必须真实存在，且含 gate.PrecheckPickup( 与
//	   gate.RecordPickupFailure( 调用——把预检/记账删掉或绕过 ⇒ 红；
//	④ 声明受理密码的面，其判定文件必须含 gate.CheckPickup(；不受理密码的
//	   面必须显式写 Judge:""（并注理由），不许留空缺省。
//
// 负对照（历史绕过入口，core#163）：transport/http/handler/share_metadata.go
// 就是当年的绕过面之一——删掉它的 gate.PrecheckPickup 调用 ⇒ ③ 红；行为级
// 负对照见 TestMetadata_EnumLockoutAfterTenMisses（删预检 ⇒ 429 断言转红）。
package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pickupFace 一个交付面的收口点承载文件（模块内相对路径）。
//
// Source 承载「入口预检 + 确证不存在记账」的文件（必须含
//
//	gate.PrecheckPickup( 与 gate.RecordPickupFailure(）。
//
// Judge 承载密码/锁定判定（gate.CheckPickup）的文件；"" = 本面不受理密码
// （理由随条目注释，不许无理由留空）。
type pickupFace struct {
	Source string
	Judge  string
}

// pickupFaces HTTP 交付面 → 收口点承载文件（契约表里 Gate != "N/A" 的 HTTP 面
// 必须全部在此登记；MCP 面是管理面读取，不走 HTTP 取件入口，不在本表）。
var pickupFaces = map[string]pickupFace{
	"GET /share/select/": {Source: "gen/handler/share/share_service.go",
		Judge: "gen/handler/share/share_service.go"},
	// 下载的密码判定在域内 GetFileWithUsage（app/share/service.go），
	// 入口预检/记账在 handler（含解析前 + 规范码解析后两道）。
	"GET /share/download": {Source: "gen/handler/share/share_service.go",
		Judge: "app/share/service.go"},
	// 免密码元数据面：不交付内容、不受理密码 ⇒ 无 CheckPickup（by design，
	// app/share/metadata_test.go 钉住「不要求密码」语义）。
	"GET /share/metadata/:code": {Source: "transport/http/handler/share_metadata.go", Judge: ""},
	"GET /preview/:code":        {Source: "app/preview/service.go", Judge: "app/preview/service.go"},
	"GET /preview/:code/thumb":  {Source: "app/preview/service.go", Judge: "app/preview/service.go"},
	// 匿名取件凭证：预检在 handler，密码/锁定判定在域内 Retrieve。
	"POST /anonymous/retrieve": {Source: "gen/handler/share_anonymous/share_anonymous_service.go",
		Judge: "app/anonymous/anonymous.go"},
	// 302 下载直链面：密码保护分享一律 401 拒绝、不受理密码输入 ⇒ 无 CheckPickup。
	"GET /anonymous/download/:code": {Source: "gen/handler/share_anonymous/share_anonymous_service.go", Judge: ""},
	// 免密码元数据面（同 metadata）。
	"GET /anonymous/search/:code": {Source: "gen/handler/share_anonymous/share_anonymous_service.go", Judge: ""},
}

// pickupMatrixPrefixes router_policy_matrix.txt 里属于取件面的路径前缀
// （只读引用 #203 产物；本片不改矩阵）。
var pickupMatrixPrefixes = []string{"/share/", "/anonymous/", "/preview/"}

// TestPickupEntryGuardCoverage 取件入口收口守卫（契约表 × 矩阵 × 源码三层）。
func TestPickupEntryGuardCoverage(t *testing.T) {
	// 契约表索引（Gate != "N/A" 的 HTTP 面 = 交付面）
	contractGate := map[string]string{}
	for _, c := range shareSurfaceContracts {
		contractGate[c.Surface] = c.Gate
	}

	// ① 契约表里的 HTTP 交付面必须登记收口点
	for _, c := range shareSurfaceContracts {
		if strings.HasPrefix(c.Surface, "MCP ") || c.Gate == "N/A" {
			continue
		}
		if _, ok := pickupFaces[c.Surface]; !ok {
			t.Errorf("交付面 %s 未在 pickupFaces 登记——新取件入口必须声明其收口点承载文件（gate.PrecheckPickup / gate.RecordPickupFailure）", c.Surface)
		}
	}
	// ② 登记面必须仍在契约表（陈旧条目）
	for s := range pickupFaces {
		if _, ok := contractGate[s]; !ok {
			t.Errorf("pickupFaces 登记的面 %s 不在取件契约表（陈旧条目）", s)
		}
	}

	// ②b 矩阵（#203）交叉核对：三个前缀下的 public 路由必须是已登记面或
	// 契约表声明的创建面（Gate == "N/A"）——矩阵里冒出新取件面 ⇒ 红。
	matrixPath := filepath.Join("..", "bootstrap", "router_policy_matrix.txt")
	if data, err := os.ReadFile(matrixPath); err != nil {
		t.Fatalf("读取路由策略矩阵失败: %v", err)
	} else {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, " | ") {
				continue
			}
			parts := strings.Split(line, "|")
			if len(parts) < 2 {
				continue
			}
			route := strings.TrimSpace(parts[0])
			fields := strings.Fields(route)
			if len(fields) != 2 {
				continue
			}
			path := fields[1]
			hit := false
			for _, p := range pickupMatrixPrefixes {
				if strings.Contains(path, p) {
					hit = true
					break
				}
			}
			if !hit || !strings.Contains(line, " public ") {
				continue
			}
			gateVal, inContract := contractGate[route]
			if !inContract {
				t.Errorf("矩阵新增取件面 %s 未在取件契约表登记（先登记契约，再在 pickupFaces 声明收口点）", route)
				continue
			}
			if gateVal != "N/A" {
				if _, ok := pickupFaces[route]; !ok {
					t.Errorf("矩阵交付面 %s 未在 pickupFaces 登记收口点承载文件", route)
				}
			}
		}
	}

	// ③④ 源码机械判据
	for s, f := range pickupFaces {
		mustContainCalls(t, f.Source, []string{"gate.PrecheckPickup(", "gate.RecordPickupFailure("},
			"面 "+s+" 的入口预检/记账")
		if f.Judge != "" {
			mustContainCalls(t, f.Judge, []string{"gate.CheckPickup("},
				"面 "+s+" 的密码/锁定判定")
		}
	}
}

// mustContainCalls 断言源文件存在且包含全部调用标记。
func mustContainCalls(t *testing.T, rel string, markers []string, what string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", rel))
	if err != nil {
		t.Errorf("%s 的承载文件 %s 读取失败: %v", what, rel, err)
		return
	}
	for _, m := range markers {
		if !strings.Contains(string(data), m) {
			t.Errorf("%s 的承载文件 %s 缺少 %s 调用——该面绕过了 pkg/gate 收口点（core#173）", what, rel, m)
		}
	}
}
