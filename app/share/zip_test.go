package share

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// panicStorage GetFileReader 必 panic 的存储桩（注入驱动 panic 场景）。
type panicStorage struct {
	storage.StorageInterface
}

func (panicStorage) GetFileReader(context.Context, string) (io.ReadCloser, int64, error) {
	panic("storage driver exploded")
}

// staticStorage 恒返回固定内容的存储桩（正常路径对照）。
type staticStorage struct {
	storage.StorageInterface
}

func (staticStorage) GetFileReader(context.Context, string) (io.ReadCloser, int64, error) {
	return io.NopCloser(strings.NewReader("hello")), 5, nil
}

// TestAssembleZipPanicReleasesReader panic 侧补偿双语义：存储驱动 panic 后，
// ① 读端必须收到 error 而非永久阻塞；② panic 原样重抛（测试里模拟外层
// GoSafe 的 recover 消费，生产环境由 GoSafe 记日志保持 panic 可见）。
func TestAssembleZipPanicReleasesReader(t *testing.T) {
	s := &Service{storage: panicStorage{}}
	pr, pw := io.Pipe()
	buildCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readErr := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(pr)
		readErr <- err
	}()

	assembled := make(chan struct{})
	rethrown := make(chan bool, 1)
	go func() {
		defer close(assembled)
		defer func() {
			// 锁定「panic 可见」半边语义：补偿后必须原样重抛（生产环境由
			// 外层 GoSafe 记日志）；吞掉不抛则此处旗标为 false。
			if r := recover(); r != nil {
				rethrown <- true
			} else {
				rethrown <- false
			}
		}() // 模拟外层 GoSafe 的 recover（消费重抛 panic）
		s.assembleZip(buildCtx, buildCtx, pw, cancel, &model.FileCode{},
			[]zipItem{{name: "a.txt", rel: "a"}, {name: "b.txt", rel: "b"}})
	}()

	select {
	case err := <-readErr:
		require.Error(t, err, "读端应收到 error 而非挂死")
		assert.Contains(t, err.Error(), "zip stream panic")
		assert.Contains(t, err.Error(), "storage driver exploded", "panic 值须原文带出")
	case <-time.After(5 * time.Second):
		t.Fatal("读端永久阻塞：panic 后写端未被 CloseWithError 释放")
	}
	<-assembled
	select {
	case ok := <-rethrown:
		assert.True(t, ok, "补偿 defer 须原样重抛 panic（panic 可见半边语义），不得吞")
	case <-time.After(5 * time.Second):
		t.Fatal("未观察到重抛：组装协程未按预期结束")
	}
}

// TestAssembleZipNormalPathCompletes 正常路径对照：抽取 assembleZip 不改变
// 组装语义——读端收全量字节后干净 EOF，无 error。
func TestAssembleZipNormalPathCompletes(t *testing.T) {
	s := &Service{storage: staticStorage{}}
	pr, pw := io.Pipe()
	buildCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readErr := make(chan error, 1)
	readData := make(chan []byte, 1)
	go func() {
		b, err := io.ReadAll(pr)
		readData <- b
		readErr <- err
	}()

	assembled := make(chan struct{})
	go func() {
		defer close(assembled)
		s.assembleZip(buildCtx, buildCtx, pw, cancel, &model.FileCode{},
			[]zipItem{{name: "a.txt", rel: "a"}, {name: "b.txt", rel: "b"}})
	}()

	select {
	case err := <-readErr:
		require.NoError(t, err)
		data := <-readData
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		require.NoError(t, err, "产物应为合法 zip")
		require.Len(t, zr.File, 2)
		assert.Equal(t, "a.txt", zr.File[0].Name)
		f, ferr := zr.File[0].Open()
		require.NoError(t, ferr)
		content, rerr := io.ReadAll(f)
		require.NoError(t, rerr)
		_ = f.Close()
		assert.Equal(t, "hello", string(content))
	case <-time.After(5 * time.Second):
		t.Fatal("正常路径读端阻塞：组装协程未正常收尾")
	}
	<-assembled
}
