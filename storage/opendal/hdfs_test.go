package opendal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 回归（2026-11-02 core#36 真机实测修）：HDFS 的 List 对**不存在的目录**
// 必须返回空列表、无错（与 S3/Azure 的「前缀不存在 = 没东西」一致）。
//
// 背景：WebHDFS 对不存在的路径返回 404 FileNotFoundException，原实现直接
// 归一化成错误 ⇒ 与 S3 后端行为不一致 ⇒ 后台 janitor 的 presign 孤儿清理
// （ListRemoteObjects("uploads/")）在 HDFS 后端下整个任务报错退出
// （而 S3 后端正常返回空集、无事可做）⇒ 孤儿清理在 HDFS 上不可用。
func TestHDFSListMissingDirReturnsEmptyList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"RemoteException":{"exception":"FileNotFoundException",` +
			`"javaClassName":"java.io.FileNotFoundException","message":"File /x not found."}}`))
	}))
	defer srv.Close()

	d, err := newHDFSDriver(map[string]string{
		"endpoint": srv.URL,
		"user":     "root",
		"root":     "pb",
	})
	if err != nil {
		t.Fatal(err)
	}
	ents, err := d.List(context.Background(), "no-such-dir")
	if err != nil {
		t.Fatalf("List 缺失目录应返回空列表无错，实际 err=%v", err)
	}
	if len(ents) != 0 {
		t.Fatalf("List 缺失目录应为空，实际 %d 条", len(ents))
	}
}

// 回归：List 对**非 404 的错误**仍必须报错（不能被 404 分支吞掉）。
func TestHDFSListServerErrorStillFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	d, err := newHDFSDriver(map[string]string{"endpoint": srv.URL, "user": "root", "root": "pb"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.List(context.Background(), "dir"); err == nil {
		t.Fatal("List 遇 500 必须报错")
	}
}
