package qrcode

import (
	"bytes"
	"strings"
	"testing"
)

// 域层行为断言（2026-10-25 审计 A5：该域此前**零测试** ⇒ 两处契约背离
// 无人守护）：
//   ① GenerateQRCodeID 的随机失败兜底恒返回**常量** ID ⇒ 存储层互相覆盖；
//   ② handler 的容量淘汰实为**随机删**（见 gen/handler/qrcode 的用例）。
// core#109 追加：size/data 上限的显式拒绝（匿名端点单请求内存 DoS 面）。

// TestGenerateQRCodeID_Unique 连续生成的 ID 必须互不相同（兜底分支的常量 ID
// 缺陷会让这里恒等）。
func TestGenerateQRCodeID_Unique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id, err := GenerateQRCodeID()
		if err != nil {
			t.Fatalf("生成 ID 失败: %v", err)
		}
		if !strings.HasPrefix(id, "qr_") {
			t.Fatalf("ID 前缀不对: %q", id)
		}
		if seen[id] {
			t.Fatalf("ID 重复: %q（第 %d 次）", id, i)
		}
		seen[id] = true
	}
}

// TestGenerateQRCode 生成 PNG（尺寸默认 + 显式 + 非法入参）。
func TestGenerateQRCode(t *testing.T) {
	png, err := GenerateQRCode("https://example.com", 0)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if len(png) < 8 || string(png[1:4]) != "PNG" {
		t.Fatalf("不是合法 PNG（前 8 字节 %x）", png[:min(8, len(png))])
	}

	png2, err := GenerateQRCode("x", 128)
	if err != nil {
		t.Fatalf("显式尺寸生成失败: %v", err)
	}
	if len(png2) == 0 {
		t.Fatal("显式尺寸产出为空")
	}

	// 空数据 ⇒ 报错
	if _, err := GenerateQRCode("", 256); err == nil {
		t.Fatal("空数据应报错")
	}
}

// TestGenerateQRCodeBase64 base64 形态（data URI 前缀）。
func TestGenerateQRCodeBase64(t *testing.T) {
	out, err := GenerateQRCodeBase64("data", 64)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if !strings.HasPrefix(out, "data:image/png;base64,") {
		t.Fatalf("base64 形态不对: %q", out[:min(40, len(out))])
	}
	if _, err := GenerateQRCodeBase64("", 64); err == nil {
		t.Fatal("空数据应报错")
	}
}

// TestValidateQRCodeData 数据有效性判定。
// ⚠️ core#109 口径变更：容量从 4296（Low 等级 Version 40）收紧为 MaxDataBytes
// （生成用的是 Medium 纠错 ⇒ 4296 长度根本生成不出来，会塌成 500）。
func TestValidateQRCodeData(t *testing.T) {
	if ValidateQRCodeData("") {
		t.Error("空数据应无效")
	}
	if !ValidateQRCodeData("ok") {
		t.Error("普通数据应有效")
	}
	if ValidateQRCodeData(strings.Repeat("a", MaxDataBytes+1)) {
		t.Error("超长数据应无效")
	}
	if !ValidateQRCodeData(strings.Repeat("a", MaxDataBytes)) {
		t.Error("边界长度应有效")
	}
}

// TestValidateGenerateParams core#109：超界取值**显式拒绝**（点名参数），
// 不静默钳制、不静默归默认。
//
// 负对照（当场可复验）：把 MaxSize 上限去掉/放大到 int 上界 ⇒
// 「size=20000 攻击形态」用例与 TestGenerateQRCode_OversizeSizeRejected 同时转红。
func TestValidateGenerateParams(t *testing.T) {
	long := strings.Repeat("a", MaxDataBytes)
	tooLong := strings.Repeat("a", MaxDataBytes+1)

	cases := []struct {
		name    string
		data    string
		size    int
		wantErr bool
		errPart string // 错误消息必须点名参数（结构事实，不依赖具体文案）
	}{
		{"data 空 ⇒ 拒绝", "", 256, true, "data"},
		{"data 恰好容量上限 ⇒ 放行", long, 256, false, ""},
		{"data 超 1 字节 ⇒ 拒绝", tooLong, 256, true, "data"},
		{"size 未传(0) ⇒ 放行走默认", "x", 0, false, ""},
		{"size 下界 64 ⇒ 放行", "x", 64, false, ""},
		{"size 上界 2048 ⇒ 放行", "x", 2048, false, ""},
		{"size 63 越下界 ⇒ 拒绝", "x", 63, true, "size"},
		{"size 2049 越上界 ⇒ 拒绝", "x", 2049, true, "size"},
		{"size=20000 攻击形态 ⇒ 拒绝", "x", 20000, true, "size"},
		{"size 负数 ⇒ 拒绝（不再静默归默认）", "x", -5, true, "size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateGenerateParams(tc.data, tc.size)
			if tc.wantErr {
				if err == nil {
					t.Fatal("应拒绝但放行")
				}
				if !strings.Contains(err.Error(), tc.errPart) {
					t.Fatalf("错误消息应点名参数 %q，实得 %q", tc.errPart, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("应放行但报错: %v", err)
			}
		})
	}
}

// TestGenerateQRCode_OversizeSizeRejected 超界 size 直接报错，不生成位图
// （修前：size=20000 ⇒ qr.PNG(20000) 分配 ~1.6GB RGBA ⇒ 匿名单请求 OOM）。
func TestGenerateQRCode_OversizeSizeRejected(t *testing.T) {
	if _, err := GenerateQRCode("https://example.com", 20000); err == nil {
		t.Fatal("size=20000 必须被拒绝（单请求内存 DoS 面）")
	}
	if _, err := GenerateQRCode(strings.Repeat("a", MaxDataBytes+1), 256); err == nil {
		t.Fatal("超容量 data 必须被拒绝（修前塌成生成失败/500）")
	}
}

// TestGenerateQRCode_BoundaryOK 边界内正常出图（PNG 魔数；容量上限恰好可生成）。
func TestGenerateQRCode_BoundaryOK(t *testing.T) {
	png, err := GenerateQRCode(strings.Repeat("a", MaxDataBytes), MaxSize)
	if err != nil {
		t.Fatalf("容量上限与 size 上界应可生成: %v", err)
	}
	if !bytes.HasPrefix(png, []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatal("应产出 PNG")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
