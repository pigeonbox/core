package qrcode

import (
	"strings"
	"testing"
)

// 域层行为断言（2026-10-25 审计 A5：该域此前**零测试** ⇒ 两处契约背离
// 无人守护）：
//   ① GenerateQRCodeID 的随机失败兜底恒返回**常量** ID ⇒ 存储层互相覆盖；
//   ② handler 的容量淘汰实为**随机删**（见 gen/handler/qrcode 的用例）。

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
func TestValidateQRCodeData(t *testing.T) {
	if ValidateQRCodeData("") {
		t.Error("空数据应无效")
	}
	if !ValidateQRCodeData("ok") {
		t.Error("普通数据应有效")
	}
	if ValidateQRCodeData(strings.Repeat("a", 4297)) {
		t.Error("超长数据（>4296）应无效")
	}
	if !ValidateQRCodeData(strings.Repeat("a", 4296)) {
		t.Error("边界长度 4296 应有效")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
