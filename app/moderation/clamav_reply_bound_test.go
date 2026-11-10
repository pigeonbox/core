package moderation

// core#120：clamd 响应读取必须限幅（Ping 与 ScanReader 两处收尾读）。
//
// 缺陷：Ping 用 io.ReadAll(conn) 从 TCP 连接无界读——clamd 地址可配，
// 指向不可信/被劫持服务时可发大流做内存放大（deadline 只兜时长不兜内存）。
//
// 判据是**区间结构事实**：错误文案必须真的带出了响应体（>4KB）但被限幅
// （<16KB）。两个失败形态都会转红：
//   - 无界读 ⇒ 响应 1MB 全量进错误文案 ⇒ 超上限红；
//   - 假服务没消费 INSTREAM / 读挂死 ⇒ 打满 deadline，错误文案不含响应体
//     ⇒ 不足下限红（本轮实录踩过这个假绿：断言只判「报错 + 不超长」时，
//     超时错误长得跟限幅错误一样绿）。
//
// 负对照（已实跑）：把两处 io.LimitReader(conn, maxClamdReplyBytes) 去掉
// （改回无界读）⇒ 两条 Blast 用例转红。

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

// fakeClamdBlast 假 clamd：读完命令后回一个**无终止标记的海量响应**
// （无 PONG、无换行），用于证明响应读取有界。payload 用可打印字符
// （%q 转义不膨胀，长度判据才稳定）。
func fakeClamdBlast(t *testing.T, payload string) string {
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
				cmd, err := r.ReadString('\x00')
				if err != nil {
					return
				}
				if strings.HasPrefix(cmd, "zINSTREAM") {
					// 先把 INSTREAM 收完（每块：4 字节大端长度 + 数据体），
					// 否则零长度块永远等不到、客户端挂死在写侧
					for {
						var sizeBuf [4]byte
						if _, err := io.ReadFull(r, sizeBuf[:]); err != nil {
							return
						}
						size := binary.BigEndian.Uint32(sizeBuf[:])
						if size == 0 {
							break
						}
						if _, err := io.CopyN(io.Discard, r, int64(size)); err != nil {
							return
						}
					}
				}
				_, _ = c.Write([]byte(payload))
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// assertBoundedReplyError 判据：错误文案真带出了响应体（含 AAA 片段、
// 长度 >4KB）且被限幅（<16KB）。
func assertBoundedReplyError(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AAA", "错误应带出响应体片段（防「超时错误」冒充限幅）")
	assert.Greater(t, len(err.Error()), 4096, "应真的读到了响应体")
	assert.Less(t, len(err.Error()), 16384, "响应必须限幅（无界读时文案≈1MB）")
}

// TestClamAVModerator_Ping_ReplyBounded Ping 的响应读取必须限幅。
func TestClamAVModerator_Ping_ReplyBounded(t *testing.T) {
	addr := fakeClamdBlast(t, strings.Repeat("A", 1<<20))
	m := NewClamAVModerator(addr, 5, 0, nil)
	assertBoundedReplyError(t, m.Ping())
}

// TestClamAVModerator_ScanReader_ReplyBounded ScanReader 的收尾读同样限幅。
func TestClamAVModerator_ScanReader_ReplyBounded(t *testing.T) {
	addr := fakeClamdBlast(t, strings.Repeat("A", 1<<20))
	m := NewClamAVModerator(addr, 5, 0, nil)
	_, err := m.ScanReader(context.Background(), strings.NewReader("hello"))
	assertBoundedReplyError(t, err)
}

// TestClamAVModerator_Ping_OK 正常 PING/PONG 不受限幅影响（防过度收紧）。
func TestClamAVModerator_Ping_OK(t *testing.T) {
	addr := fakeClamd(t)
	m := NewClamAVModerator(addr, 5, 0, nil)
	assert.NoError(t, m.Ping())
}
