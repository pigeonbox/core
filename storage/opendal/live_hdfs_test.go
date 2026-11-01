//go:build live

// HDFS 驱动真机集成测试（真 WebHDFS，非 mock）。
//
// 与 live_azure_test.go 同族，覆盖 2026-11-02 core#36 的 HDFS 侧验收。
//
// 覆盖 2026-11-02 真机实测修掉的缺陷（回归护栏）：
//
//	· List 对「不存在的目录」返回 404 错误，而 S3/Azure 的同一条调用语义是
//	  **空列表** ⇒ 跨后端行为不一致 ⇒ 后台 janitor 的 presign 孤儿清理
//	  （ListRemoteObjects("uploads/")）在 HDFS 后端下整个任务报错退出。
//
// 前置：scripts/live-storage-services.sh 起 apache/hadoop 单节点 + WebHDFS。
// 环境变量：
//
//	PB_LIVE_HDFS_ENDPOINT 默认 http://127.0.0.1:9870
//	PB_LIVE_HDFS_USER     默认 root
package opendal

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestLiveHDFS 对真 WebHDFS 跑通驱动全链路。
func TestLiveHDFS(t *testing.T) {
	endpoint := liveEnv("PB_LIVE_HDFS_ENDPOINT", "http://127.0.0.1:9870")
	user := liveEnv("PB_LIVE_HDFS_USER", "root")

	if !liveEndpointReachable(endpoint + "/webhdfs/v1/?op=LISTSTATUS") {
		t.Skipf("SKIP: 真 HDFS（WebHDFS）不可达：%s —— 起 hdfs 后重跑", endpoint)
	}

	ctx := context.Background()
	op, err := New(Config{
		Scheme: SchemeHDFS,
		Options: map[string]string{
			"endpoint": endpoint,
			"user":     user,
			"root":     "pb-live",
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := Probe(ctx, op); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if err := op.CreateDir(ctx, "dir-a"); err != nil {
		t.Fatalf("CreateDir: %v", err)
	}
	body := []byte("live-hdfs-check-9527")
	if err := op.Write(ctx, "dir-a/file1.txt", body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := op.Read(ctx, "dir-a/file1.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("Read 内容不符：%q", string(got))
	}
	md, err := op.Stat(ctx, "dir-a/file1.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if md.Size != int64(len(body)) {
		t.Fatalf("Stat size=%d want=%d", md.Size, len(body))
	}
	// 缺失文件 ⇒ os.ErrNotExist 契约
	if _, err := op.Stat(ctx, "dir-a/missing.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat 缺失文件应包装 os.ErrNotExist，实际=%v", err)
	}

	// ── 回归护栏：缺失前缀 ⇒ 空列表、无错（与 S3/Azure 语义一致）
	ents, err := op.List(ctx, "no-such-dir")
	if err != nil {
		t.Fatalf("List 缺失目录应返回空列表无错，实际 err=%v", err)
	}
	if len(ents) != 0 {
		t.Fatalf("List 缺失目录应为空，实际 %d 条", len(ents))
	}

	// 列举真实对象（List 的 Path 语义 = 相对所列目录的名字，与 S3 一致）
	ents, err = op.List(ctx, "dir-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ents) != 1 || ents[0].Path != "file1.txt" {
		t.Fatalf("List 结果不符：%+v", ents)
	}

	// 跨块大文件
	big := strings.Repeat("Z", 3*1024*1024)
	if err := op.WriteStream(ctx, "dir-a/big.bin", strings.NewReader(big), int64(len(big))); err != nil {
		t.Fatalf("WriteStream: %v", err)
	}
	md, err = op.Stat(ctx, "dir-a/big.bin")
	if err != nil {
		t.Fatalf("Stat big: %v", err)
	}
	if md.Size != int64(len(big)) {
		t.Fatalf("大文件 size=%d want=%d", md.Size, len(big))
	}

	// 清理
	if err := op.RemoveAll(ctx, "dir-a"); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if op.Exists(ctx, "dir-a/file1.txt") {
		t.Fatal("RemoveAll 后仍存在")
	}
}
