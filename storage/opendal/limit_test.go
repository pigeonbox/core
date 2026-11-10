package opendal

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// countingReader 记录实际交付字节数的读端（「有界消费」判据用）。
type countingReader struct {
	data  []byte
	off   int
	count int
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	r.count += n
	return n, nil
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// TestReadAllLimited_Boundary（core#118 正判据）：恰好 max 字节放行；max+1 字节
// 显式报 ErrReadTooLarge、**不返回半截数据**（超限报错而非静默截断），且
// **有界消费**（读端交付不超过 max+1 字节——内存保护的结构事实，独立于错误
// 语义：只把判超限改成事后检查、去掉 LimitReader 时错误照报但整读已发生）。
//
// 负对照（改错转红）：
//   - 去掉 LimitReader（io.ReadAll 裸读）⇒ 有界消费断言转红（消费=全部字节）；
//   - 回退为裸 io.ReadAll 且不判超限（= 原缺陷形态）⇒ 超限错误断言与半截数据
//     断言同时转红。
func TestReadAllLimited_Boundary(t *testing.T) {
	const max = 8

	got, err := ReadAllLimited(strings.NewReader("12345678"), max)
	if err != nil || string(got) != "12345678" {
		t.Fatalf("恰好 max 字节应放行: got=%q err=%v", got, err)
	}

	got, err = ReadAllLimited(strings.NewReader("1234567"), max)
	if err != nil || string(got) != "1234567" {
		t.Fatalf("少于 max 应放行: got=%q err=%v", got, err)
	}

	cr := &countingReader{data: []byte(strings.Repeat("x", max+100))}
	got, err = ReadAllLimited(cr, max)
	if !errors.Is(err, ErrReadTooLarge) {
		t.Fatalf("超过 max 必须报 ErrReadTooLarge: err=%v", err)
	}
	if got != nil {
		t.Fatalf("超限不得返回半截数据: got=%q", got)
	}
	if cr.count > max+1 {
		t.Fatalf("限量整读必须有界消费（内存保护）：实际消费 %d 字节，期望 ≤ %d", cr.count, max+1)
	}
}

// TestReadAllLimited_ReadError 底层读错误透传（限量层不吞错）。
func TestReadAllLimited_ReadError(t *testing.T) {
	if _, err := ReadAllLimited(errReader{err: errors.New("boom")}, 8); err == nil {
		t.Fatal("底层读错误必须透传")
	}
}
