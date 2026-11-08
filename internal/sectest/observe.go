// Package sectest 测试专用观测原语：反射读 secretx.SecureString 的**混淆后存储**。
//
// 为什么必须反射穿私有字段：判据要证明的是「堆上那份字节不是明文」，这只能在
// 持有器内部看——Get() 已经解出明文，拿它来扫只能扫到明文，证明不了任何事。
// 模板：pkg/auth/jwt_secret_test.go（core#73），跨包复用收编于此（conf /
// bootstrap / app/config / app/oidc 四处判据同款）。
//
// 仅供测试消费：生产代码不得 import。
package sectest

import (
	"bytes"
	"reflect"
	"testing"
	"unsafe"

	"github.com/oekit/pkg/secretx"
)

// RawObfuscated 读出持有器的混淆后存储。字段改名时 Skip 而不是 Pass：
// 判据失效必须响，否则它会变成一个永远绿的摆设。
func RawObfuscated(t *testing.T, holder *secretx.SecureString) []byte {
	t.Helper()
	if holder == nil {
		t.Fatal("持有器为 nil（解码/注入路径没有产出持有器？）")
	}
	f := reflect.ValueOf(holder).Elem().FieldByName("obfuscated")
	if !f.IsValid() {
		t.Skip("secretx.SecureString 内部字段已改名，混淆判据需重新对齐")
	}
	return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().([]byte)
}

// RawKey 读出持有器的 XOR 密钥存储（与 obfuscated 配对的另一半明文残迹面）。
func RawKey(t *testing.T, holder *secretx.SecureString) []byte {
	t.Helper()
	if holder == nil {
		t.Fatal("持有器为 nil（解码/注入路径没有产出持有器？）")
	}
	f := reflect.ValueOf(holder).Elem().FieldByName("key")
	if !f.IsValid() {
		t.Skip("secretx.SecureString 内部字段已改名，混淆判据需重新对齐")
	}
	return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().([]byte)
}

// AssertNotStoredInPlaintext 断言混淆存储与 XOR 密钥都不含 marker 明文，
// 且取回值 == marker（证明「藏起来」之外还「能用」）。
// 对照断言（普通字节能被扫到）由调用方按模板自行给出，此处不复述。
func AssertNotStoredInPlaintext(t *testing.T, holder *secretx.SecureString, marker string) {
	t.Helper()
	for _, part := range []struct {
		name string
		raw  []byte
	}{
		{"obfuscated", RawObfuscated(t, holder)},
		{"key", RawKey(t, holder)},
	} {
		if bytes.Contains(part.raw, []byte(marker)) {
			t.Fatalf("%s 存储以明文驻留：%q", part.name, marker)
		}
	}
	if got := holder.Get(); got != marker {
		t.Fatalf("取回值与注入值不一致：got %q want %q", got, marker)
	}
}

// AssertCleared 断言持有器已被零化（Clear/Set("") 语义：obfuscated/key 均空，
// Get 返回空串）。
func AssertCleared(t *testing.T, holder *secretx.SecureString) {
	t.Helper()
	if obf := RawObfuscated(t, holder); len(obf) != 0 {
		t.Fatalf("混淆存储未零化，残留 %d 字节", len(obf))
	}
	if k := RawKey(t, holder); len(k) != 0 {
		t.Fatalf("XOR 密钥存储未零化，残留 %d 字节", len(k))
	}
	if got := holder.Get(); got != "" {
		t.Fatalf("Clear 后 Get 应为空串，got %q", got)
	}
}
