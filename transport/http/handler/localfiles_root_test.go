// core#113 判据：localRootQuery 的 root 解析失败**静默落 root 0**——
// DELETE /admin/local-files 是破坏性端点，root 打错（非数字/空格/URL 编码错）
// 被悄悄改成 root 0 ⇒ 删除打到另一个根目录上的同名相对路径，接口照样回
// 「删除成功」（core#102「解析失败必须显式失败」同族）。
//
// 判据：非数字/越界 ⇒ 400 + 业务码 10001 + 消息点名 root；缺省/空串仍 0
// （历史默认根不动）；roots 未配置时不判越界（交服务端「未启用」引导语）。
//
// 负对照（当场可复验）：把 parseLocalRoot 改回静默落 0 的旧实现
// （strconv.Atoi 失败 return 0, true）⇒ 下列「应拒绝」用例全部转红。
package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"

	"github.com/pigeonbox/core/conf"
)

// withLocalImportRoots 注入 n 个可解析的白名单根目录，结束后恢复全局配置。
func withLocalImportRoots(t *testing.T, n int) {
	t.Helper()
	old := conf.GetGlobalConfig()
	roots := make([]string, 0, n)
	for i := 0; i < n; i++ {
		roots = append(roots, t.TempDir())
	}
	conf.SetGlobalConfig(&conf.AppConfiguration{
		Upload: conf.UploadConfig{LocalImport: conf.LocalImportConfig{Enabled: true, Roots: roots}},
	})
	t.Cleanup(func() { conf.SetGlobalConfig(old) })
}

func newRootContext(query string) *app.RequestContext {
	c := app.NewContext(0)
	if query != "" {
		c.Request.URI().SetQueryString(query)
	}
	return c
}

// assertParam400 断言响应是 400 + 10001 全站信封且消息点名参数。
func assertParam400(t *testing.T, c *app.RequestContext, param string) {
	t.Helper()
	if code := c.Response.StatusCode(); code != 400 {
		t.Fatalf("HTTP 应 400，实得 %d（body %s）", code, c.Response.Body())
	}
	var body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		TraceID string `json:"trace_id"`
	}
	if err := json.Unmarshal(c.Response.Body(), &body); err != nil {
		t.Fatalf("错误响应应是 JSON 信封: %v", err)
	}
	if body.Code != 10001 {
		t.Fatalf("业务码应 10001，实得 %d", body.Code)
	}
	if !strings.Contains(body.Message, param) {
		t.Fatalf("消息应点名参数 %s，实得 %q", param, body.Message)
	}
	if body.TraceID == "" {
		t.Fatal("全站信封应带 trace_id")
	}
}

func TestParseLocalRoot(t *testing.T) {
	t.Run("缺省/空串/合法数字", func(t *testing.T) {
		withLocalImportRoots(t, 3)
		cases := []struct {
			query string
			want  int
		}{
			{"", 0},       // 未传 ⇒ 默认根 0（历史口径不动）
			{"root=", 0},  // 空值同「没传」
			{"root=0", 0}, // 下界
			{"root=2", 2}, // 上界
			{"root=1", 1},
		}
		for _, tc := range cases {
			c := newRootContext(tc.query)
			got, ok := parseLocalRoot(c)
			if !ok {
				t.Fatalf("%q 应放行但失败（body %s）", tc.query, c.Response.Body())
			}
			if got != tc.want {
				t.Fatalf("%q ⇒ root=%d，want %d", tc.query, got, tc.want)
			}
		}
	})

	t.Run("应拒绝：非数字", func(t *testing.T) {
		withLocalImportRoots(t, 3)
		for _, q := range []string{"root=abc", "root= 2", "root=2 ", "root=2.5", "root=0x2", "root=1e3"} {
			c := newRootContext(q)
			if _, ok := parseLocalRoot(c); ok {
				t.Fatalf("%q 应拒绝但放行（修前静默落 root 0 ⇒ 删错根目录）", q)
			}
			assertParam400(t, c, "root")
		}
	})

	t.Run("应拒绝：越界", func(t *testing.T) {
		withLocalImportRoots(t, 3)
		for _, q := range []string{"root=3", "root=99", "root=-1"} {
			c := newRootContext(q)
			if _, ok := parseLocalRoot(c); ok {
				t.Fatalf("%q 越界应拒绝但放行", q)
			}
			assertParam400(t, c, "root")
		}
	})

	t.Run("roots 未配置：不做越界判定（交服务端未启用引导语）", func(t *testing.T) {
		old := conf.GetGlobalConfig()
		conf.SetGlobalConfig(&conf.AppConfiguration{})
		t.Cleanup(func() { conf.SetGlobalConfig(old) })
		c := newRootContext("root=5")
		got, ok := parseLocalRoot(c)
		if !ok {
			t.Fatalf("roots 为空时不应在解析层拒（body %s）", c.Response.Body())
		}
		if got != 5 {
			t.Fatalf("root=5 应原样交服务端，实得 %d", got)
		}
	})
}

// TestAdminListLocalFiles_InvalidRoot400 handler 级：root 解析失败在触达服务前
// 就 400 返回（不注入 shareSvc 也不 panic = 未触达 getShareService）。
func TestAdminListLocalFiles_InvalidRoot400(t *testing.T) {
	withLocalImportRoots(t, 2)
	c := newRootContext("root=abc")
	AdminListLocalFiles(context.Background(), c)
	assertParam400(t, c, "root")
}

// TestAdminDeleteLocalFile_InvalidRoot400 删除端点同判据（破坏性端点打错根
// 目录 = 删错文件，必须显式拒绝而不是静默落 root 0）。
func TestAdminDeleteLocalFile_InvalidRoot400(t *testing.T) {
	withLocalImportRoots(t, 2)
	c := newRootContext("root=abc&path=a/b.txt")
	AdminDeleteLocalFile(context.Background(), c)
	assertParam400(t, c, "root")
}
