// 扫描器：以 gen/router 生成物为真相源，枚举全部路由及其鉴权链。
//
// 解析两类生成文件：
//   - <pkg>.go（Register）：Group 层级声明 + 路由注册行
//   - middleware.go：各 _xxxMw() 中间件组实际挂载的中间件
//
// 鉴权推导：路由 → 叶子 Group → 沿祖先链收集全部中间件调用 → 词表映射。
package policy

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	routeRe  = regexp.MustCompile(`(\w+)\.(GET|POST|PUT|DELETE|PATCH)\("([^"]+)",\s*append\((\w+)\(\),\s*([\w]+)\.([\w]+)\)`)
	mwDefRe  = regexp.MustCompile(`func (\w+)\(\) \[\]app\.Handler(?:Func)? \{\n(.*?)\n\}`)
	mwCallRe = regexp.MustCompile(`(?:middleware|gate)\.[A-Za-z0-9]+`)
	groupRe  = regexp.MustCompile(`^\s*(\w+)\s*:?=.*\.Group\("([^"]+)"(?:,\s*(.*?))?\)\s*$`)
	mwVarRe  = regexp.MustCompile(`(\w+)\(\)`)
)

// ScanRoutes 扫描 genRoot（…/gen）下的 router 生成物，返回全部路由。
func ScanRoutes(genRoot string) ([]Route, error) {
	routerDir := filepath.Join(genRoot, "router")
	entries, err := os.ReadDir(routerDir)
	if err != nil {
		return nil, fmt.Errorf("policy: 读取 %s: %w", routerDir, err)
	}

	midMap := map[string]map[string]map[string]bool{} // pkg -> mwVar -> middleware names
	groups := map[string]map[string]struct {          // pkg -> groupVar -> 父 + mw 组
		parent string
		mws    []string
	}{}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pkg := e.Name()
		midMap[pkg] = map[string]map[string]bool{}
		groups[pkg] = map[string]struct {
			parent string
			mws    []string
		}{}
		dir := filepath.Join(routerDir, pkg)
		goFiles, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range goFiles {
			text, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			content := string(text)
			for m := mwDefRe.FindAllStringSubmatch(content, -1); len(m) > 0; m = nil {
				break
			}
			for _, m := range mwDefRe.FindAllStringSubmatch(content, -1) {
				set := map[string]bool{}
				for _, name := range mwCallRe.FindAllString(m[2], -1) {
					set[name] = true
				}
				midMap[pkg][m[1]] = set
			}
			sc := bufio.NewScanner(strings.NewReader(content))
			for sc.Scan() {
				line := sc.Text()
				if !strings.Contains(line, ".Group(") {
					continue
				}
				g := groupRe.FindStringSubmatch(line)
				if g == nil {
					continue
				}
				var mws []string
				for _, mv := range mwVarRe.FindAllStringSubmatch(g[3], -1) {
					mws = append(mws, mv[1])
				}
				groups[pkg][g[1]] = struct {
					parent string
					mws    []string
				}{g[2], mws}
			}
		}
	}

	chainAuth := func(pkg, leafGroup, leafMW string) string {
		mids := map[string]bool{}
		for name := range midMap[pkg][leafMW] {
			mids[name] = true
		}
		cur, seen := leafGroup, map[string]bool{}
		for {
			g, ok := groups[pkg][cur]
			if !ok || seen[cur] {
				break
			}
			seen[cur] = true
			for _, mw := range g.mws {
				for name := range midMap[pkg][mw] {
					mids[name] = true
				}
			}
			cur = g.parent
		}
		has := func(suffix string) bool {
			for name := range mids {
				if strings.HasSuffix(name, suffix) {
					return true
				}
			}
			return false
		}
		switch {
		case has("AdminMiddleware"), has("AdminOrAdminKey"):
			return "admin"
		case has("AuthMiddleware"):
			return "user"
		case has("UserOrAPIKey"):
			return "user_or_apikey"
		case has("OptionalIdentity"), has("OptionalAuthMiddleware"):
			return "optional"
		default:
			return "anonymous"
		}
	}

	seen := map[string]bool{}
	var out []Route
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pkg := e.Name()
		dir := filepath.Join(routerDir, pkg)
		goFiles, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range goFiles {
			if filepath.Base(f) == "middleware.go" {
				continue
			}
			text, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			for _, m := range routeRe.FindAllStringSubmatch(string(text), -1) {
				r := Route{
					Pkg:       pkg,
					Method:    m[2],
					Path:      m[3],
					Handler:   m[6],
					ChainAuth: chainAuth(pkg, m[1], m[4]),
				}
				key := r.Method + " " + r.Pkg + "/" + r.Handler + " " + r.Path
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Pkg+a.Handler+a.Method < b.Pkg+b.Handler+b.Method
	})
	return out, nil
}
