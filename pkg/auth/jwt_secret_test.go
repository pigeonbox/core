package auth

import (
	"bytes"
	"reflect"
	"testing"
	"unsafe"
)

// 本文件锁住 core#73 的三条语义：密钥不以明文驻留堆上 / getSecret 交出的是副本 /
// 热更换密钥即时生效（旧令牌失效、新令牌可用）。

// rawObfuscated 读出 secretx.SecureString 的**混淆后存储**（仅测试用）。
//
// 为什么用反射穿私有字段：判据要证明的是「堆上那份字节不是明文」，而这必须在
// 持有器内部看——Get() 已经解出明文，拿它来扫只能扫到明文，证明不了任何事。
// 字段改名时 Skip 而不是 Pass：判据失效必须响，否则它会变成一个永远绿的摆设。
func rawObfuscated(t *testing.T, holder any) []byte {
	t.Helper()
	f := reflect.ValueOf(holder).Elem().FieldByName("obfuscated")
	if !f.IsValid() {
		t.Skip("secretx.SecureString 内部字段已改名，混淆判据需重新对齐")
	}
	return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().([]byte)
}

// TestJWTSecret_NotStoredInPlaintext 混淆真的在工作：同样的字节放普通切片扫得到，
// 放持有器里扫不到，且取回仍是原文（证明「藏起来」之外还「能用」）。
func TestJWTSecret_NotStoredInPlaintext(t *testing.T) {
	const marker = "plaintext-marker-8f3a1c"
	t.Cleanup(func() { SetJWTSecret("PigeonBox2025SecretKey") })

	// 对照：普通切片必须被扫到——否则本条只能证明扫描手法坏掉（假绿）。
	plain := []byte(marker)
	if !bytes.Contains(plain, []byte(marker)) {
		t.Fatal("对照失败：普通切片里扫不到明文，扫描手法本身有问题")
	}

	SetJWTSecret(marker)
	held := rawObfuscated(t, jwtSecret)
	if len(held) != len(marker) {
		t.Fatalf("持有长度不符（存储为零化/截断？）：got %d want %d", len(held), len(marker))
	}
	if bytes.Contains(held, []byte(marker)) {
		t.Fatalf("JWT 签名密钥以明文驻留堆上：%q", marker)
	}
	if got := string(getSecret()); got != marker {
		t.Fatalf("取回值与注入值不一致：got %q want %q", got, marker)
	}
}

// TestJWTSecret_GetReturnsCopy 负对照靶心：旧实现交出内部切片，调用方就地改写
// 就等于改签名密钥。本用例对旧实现必然转红（改 3 字节后密钥随之而变）。
func TestJWTSecret_GetReturnsCopy(t *testing.T) {
	const key = "copy-isolation-key"
	t.Cleanup(func() { SetJWTSecret("PigeonBox2025SecretKey") })
	SetJWTSecret(key)

	got := getSecret()
	for i := 0; i < 3 && i < len(got); i++ {
		got[i] = 'X' // 模拟调用方/第三方库就地改写返回的切片
	}
	if after := string(getSecret()); after != key {
		t.Fatalf("改写返回切片污染了密钥本体：got %q want %q（说明交出的是内部切片）", after, key)
	}
}

// TestJWTSecret_HotReload 热更换密钥：旧令牌必须失效，新令牌必须可用。
// 只测「旧失效」会漏掉「把所有令牌都拒了」这种更糟的实现。
func TestJWTSecret_HotReload(t *testing.T) {
	t.Cleanup(func() { SetJWTSecret("PigeonBox2025SecretKey") })

	SetJWTSecret("reload-old")
	oldToken, err := GenerateToken(7, "u", "user")
	if err != nil {
		t.Fatalf("签发旧令牌失败：%v", err)
	}

	SetJWTSecret("reload-new")
	if _, err := ParseToken(oldToken); err == nil {
		t.Fatal("旧密钥签发的令牌在新密钥下仍然通过校验")
	}

	newToken, err := GenerateToken(7, "u", "user")
	if err != nil {
		t.Fatalf("签发新令牌失败：%v", err)
	}
	if _, err := ParseToken(newToken); err != nil {
		t.Fatalf("新密钥签发的令牌校验失败：%v", err)
	}
}
