package qrcode

import (
	"encoding/base64"
	"fmt"

	"github.com/oekit/kitx/uidgen"
	"github.com/skip2/go-qrcode"
)

// 生成参数边界（core#109）：/qrcode 系列端点匿名可达，size 此前无上限——
// size=20000 单请求分配 ~size²×4 字节 RGBA 缓冲（≈1.6GB）⇒ 单请求 OOM
// （限流桶只限频率，挡不住单发大图；base64 返回再放大 4/3）。
// 口径与 utils.CheckUploadSize 同款：**显式拒绝，不静默钳制**。
const (
	// DefaultSize 未指定 size（0）时的默认尺寸（像素，历史口径）。
	DefaultSize = 256
	// MinSize size 下限（像素）。过小的位图不可用，且区间外一律显式拒绝。
	MinSize = 64
	// MaxSize size 上限（像素）。2048²×4 ≈ 16MB RGBA/请求，单请求安全线内。
	MaxSize = 2048
	// MaxDataBytes data 字节上限 = go-qrcode Medium 纠错等级 Version 40 的
	// 字节容量。超长在生成阶段报错（handler 侧塌成 500）——收在参数校验层给 400。
	MaxDataBytes = 2331
)

// ValidateGenerateParams 校验生成参数（core#109）。
//
// 拒绝口径（与 core#102/#104/#123 家族对齐）：非法取值显式报错（点名参数），
// 不静默钳制、不静默归默认。size==0 = 「未传」⇒ 合法（生成时取 DefaultSize）。
func ValidateGenerateParams(data string, size int) error {
	if data == "" {
		return fmt.Errorf("参数 data 不能为空")
	}
	if len(data) > MaxDataBytes {
		return fmt.Errorf("参数 data 过长：%d 字节，上限 %d 字节（QR Version 40 Medium 纠错容量）", len(data), MaxDataBytes)
	}
	if size != 0 && (size < MinSize || size > MaxSize) {
		return fmt.Errorf("参数 size 非法：%d，合法区间 %d..%d（0 = 默认 %d）", size, MinSize, MaxSize, DefaultSize)
	}
	return nil
}

// GenerateQRCode 生成二维码PNG图片
// @param data 二维码数据内容
// @param size 二维码尺寸（像素）
// @return []byte PNG图片数据
// @return error 错误信息
func GenerateQRCode(data string, size int) ([]byte, error) {
	if err := ValidateGenerateParams(data, size); err != nil {
		return nil, err
	}

	if size <= 0 {
		size = DefaultSize
	}

	// 生成二维码
	qr, err := qrcode.New(data, qrcode.Medium)
	if err != nil {
		return nil, fmt.Errorf("生成二维码失败: %v", err)
	}

	// 设置二维码边框
	qr.DisableBorder = false

	// 生成PNG图片
	pngData, err := qr.PNG(size)
	if err != nil {
		return nil, fmt.Errorf("生成PNG图片失败: %v", err)
	}

	return pngData, nil
}

// GenerateQRCodeBase64 生成Base64编码的二维码图片
// @param data 二维码数据内容
// @param size 二维码尺寸（像素）
// @return string Base64编码的PNG图片数据
// @return error 错误信息
func GenerateQRCodeBase64(data string, size int) (string, error) {
	pngData, err := GenerateQRCode(data, size)
	if err != nil {
		return "", err
	}

	// 转换为Base64
	base64Data := fmt.Sprintf("data:image/png;base64,%s",
		base64.StdEncoding.EncodeToString(pngData))

	return base64Data, nil
}

// ValidateQRCodeData 验证二维码数据有效性
// @param data 二维码数据
// @return bool 是否有效
func ValidateQRCodeData(data string) bool {
	if data == "" {
		return false
	}

	// 检查数据长度（二维码有容量限制）：与生成口径同源 MaxDataBytes
	//（Medium 纠错的 Version 40 字节容量；此前写死 4296 是 Low 容量，
	// 该长度在 Medium 下根本生成不出来）。core#109。
	if len(data) > MaxDataBytes {
		return false
	}

	return true
}

// GenerateQRCodeID 生成二维码唯一ID
//
// ⚠️ 2026-10-25 审计 A5：原实现的随机失败兜底是
// `fmt.Sprintf("qr_%x", make([]byte, 8))` ⇒ **恒返回常量** "qr_0000000000000000"
// ⇒ 该分支下所有二维码 ID 相同 ⇒ 存储层 `QRCodeStore[id] = qr` **互相覆盖**
// ⇒ 先生成的二维码取不到（拿到的是后一个的内容）。
//
// ⇒ 随机源失败**显式报错**（不悄悄给一个会撞的 ID）：crypto/rand 读取失败意味着
// 系统熵源异常，此时返回错误比返回"一个会撞的 ID"安全。
//
// @return string 二维码ID
// @return error 随机源不可用
func GenerateQRCodeID() (string, error) {
	// qrcodeIDEntropyBytes 二维码 ID 的熵预算：8 字节 = 64 bit 熵 ⇒ 2^64 防碰撞
	// 预算（core#86 收编 kitx/uidgen 时定长于此；wire 格式 "qr_"+16 hex 逐字不变）。
	const qrcodeIDEntropyBytes = 8
	id, err := uidgen.RandomHexE(qrcodeIDEntropyBytes)
	if err != nil {
		return "", fmt.Errorf("生成二维码 ID 失败（随机源不可用）: %w", err)
	}
	return "qr_" + id, nil
}
