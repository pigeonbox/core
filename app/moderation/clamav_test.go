package moderation

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClamd 假 clamd：按内容是否包含 "EICAR" 返回 OK / FOUND
func fakeClamd(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				r := bufio.NewReader(c)
				// 命令以 \x00 结尾（zINSTREAM\0 / zPING\0）
				cmd, err := r.ReadString('\x00')
				if err != nil {
					return
				}
				if strings.HasPrefix(cmd, "zPING") {
					_, _ = c.Write([]byte("PONG\n"))
					return
				}
				if !strings.HasPrefix(cmd, "zINSTREAM") {
					_, _ = c.Write([]byte("UNKNOWN COMMAND\n"))
					return
				}
				var content strings.Builder
				for {
					var sizeBuf [4]byte
					if _, err := io.ReadFull(r, sizeBuf[:]); err != nil {
						return
					}
					size := binary.BigEndian.Uint32(sizeBuf[:])
					if size == 0 {
						break
					}
					chunk := make([]byte, size)
					if _, err := io.ReadFull(r, chunk); err != nil {
						return
					}
					content.Write(chunk)
				}
				if strings.Contains(content.String(), "EICAR") {
					_, _ = c.Write([]byte("stream: Eicar-Test-Signature FOUND\n"))
				} else {
					_, _ = c.Write([]byte("stream: OK\n"))
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func TestClamAVModerator_ScanReader(t *testing.T) {
	addr := fakeClamd(t)
	m := NewClamAVModerator(addr, 5, 0, nil, false)

	// 干净内容
	sig, err := m.ScanReader(context.Background(), strings.NewReader("hello world"))
	require.NoError(t, err)
	assert.Empty(t, sig)

	// 命中签名
	sig, err = m.ScanReader(context.Background(), strings.NewReader("EICAR-TEST-SIGNATURE"))
	require.NoError(t, err)
	assert.Equal(t, "Eicar-Test-Signature", sig)
}

func TestClamAVModerator_InspectFile(t *testing.T) {
	addr := fakeClamd(t)
	opener := &fakeOpener{content: "clean content"}
	m := NewClamAVModerator(addr, 5, 1024, opener, false)

	// 正常扫描放行
	assert.Equal(t, VerdictAllow, m.InspectFile(context.Background(), UploadMeta{
		StoragePath: "uploads/x.txt", FileName: "x.txt", Size: 12,
	}))

	// 无路径放行
	assert.Equal(t, VerdictAllow, m.InspectFile(context.Background(), UploadMeta{FileName: "x"}))

	// 超过上限跳过
	assert.Equal(t, VerdictAllow, m.InspectFile(context.Background(), UploadMeta{
		StoragePath: "uploads/x.txt", FileName: "x.txt", Size: 2048,
	}))

	// 命中签名拒绝
	opener.content = "EICAR payload"
	assert.Equal(t, VerdictReject, m.InspectFile(context.Background(), UploadMeta{
		StoragePath: "uploads/virus.txt", FileName: "virus.txt", Size: 13,
	}))
}

func TestClamAVModerator_FailOpen(t *testing.T) {
	// 不存在的地址 → 连接失败 → fail-open 放行
	m := NewClamAVModerator("127.0.0.1:1", 1, 0, &fakeOpener{content: "x"}, false)
	assert.Equal(t, VerdictAllow, m.InspectFile(context.Background(), UploadMeta{
		StoragePath: "uploads/x.txt", FileName: "x.txt", Size: 1,
	}))
}

func TestCombinedModerator(t *testing.T) {
	addr := fakeClamd(t)
	word := NewWordListModerator([]string{"badword"}, "reject")
	opener := &fakeOpener{content: "EICAR"}
	clam := NewClamAVModerator(addr, 5, 0, opener, false)
	comb := NewCombinedModerator(word, clam)

	// 文本走词表
	assert.Equal(t, VerdictReject, comb.InspectText(context.Background(), "has badword inside"))
	assert.Equal(t, VerdictAllow, comb.InspectText(context.Background(), "clean"))

	// 文件走 ClamAV
	assert.Equal(t, VerdictReject, comb.InspectFile(context.Background(), UploadMeta{
		StoragePath: "uploads/v.txt", FileName: "v.txt", Size: 5,
	}))
	opener.content = "clean"
	assert.Equal(t, VerdictAllow, comb.InspectFile(context.Background(), UploadMeta{
		StoragePath: "uploads/ok.txt", FileName: "ok.txt", Size: 5,
	}))
}

// fakeOpener 测试用 ContentOpener
type fakeOpener struct{ content string }

func (f *fakeOpener) GetFileReader(context.Context, string) (io.ReadCloser, int64, error) {
	return io.NopCloser(strings.NewReader(f.content)), int64(len(f.content)), nil
}
