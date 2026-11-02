package share

import (
	"encoding/base64"
	"fmt"
	"strings"

	pkgerrors "github.com/pigeonbox/core/pkg/errors"
)

// =====================================================================
// 口令型 E2E 的 wrapped_key 形状校验（core#30 补缺）
//
// ## 为什么服务端要校验（明明说「零知识，只存只发」）
//
// 零知识说的是**不持有口令 ⇒ 无法解密**；它**不等于**「原样吞下任意字节」。
// 不校验的真实代价：
//
//  1. **存储放大**：wrapped_key 无长度上限 ⇒ 每个分享可塞任意大的字符串
//     （file_codes 一行 ⇒ 共享 DB 与备份包体积）。
//  2. **跨库行为不一致**：列宽 `size:256` ⇒ MySQL 严格模式**直接报错**、
//     宽松模式**静默截断**（⇒ 取件人解不开，但服务端看不出异常）；
//     SQLite 不限长 ⇒ 同一份请求在不同库上行为不同。
//  3. **传输放大**：详情面把 wrapped_key 原样回传 ⇒ 一次分享可放大成
//     任意大的响应体。
//
// ## 校验只做「形状」，不做「内容」
//
// 校验的是**编码与长度**（标准 base64 + 解码后字节数在区间内），
// **不解密、不解析语义、不校验信封版本** ⇒
//   - 零知识保持：服务端没有口令，任何校验都不可能解密内容；
//   - 不阻碍演进：信封版本升级（如 0x01 → 0x02）不会因为服务端钉死版本而被拒。
//
// ## 为什么是宽松区间而不是钉死 77 字节
//
// 契约侧信封现状是 `0x01||salt16||nonce12||ct32||tag16` = 77B（base64 104 字符），
// 但**内容密钥长度与信封版本都可能演进** ⇒ 服务端钉死 77B ⇒ 未来格式一升级
// 旧服务端就把合法分享全拒了（且前端毫无补救路径）。
// ⇒ 只保「一定不是垃圾」的下界与「一定不是存储放大」的上界。
//
// =====================================================================

const (
	// wrappedKeyMaxBytes 解码后字节数上限（信封现状 77B；留足演进余量）。
	// 超过 ⇒ 拒（防存储/传输放大）。
	wrappedKeyMaxBytes = 512
	// 说明：**没有下界检查**——空串在函数开头就按「未提供」返回了；
	// 而标准 base64 下「非空串 ⇒ 解码至少 1 字节」⇒ 下界是不可达的死分支。
)

// ValidateWrappedKey 校验口令型 E2E 的信封密钥形状。
//
// 空串 = 「不带信封」⇒ 合法（明文分享 / 旧 URL-hash-key 形态）。
// 非空 ⇒ 必须是**标准 base64**（带 padding，与前端 utils/e2e.ts 的 toB64 同款）
// 且解码后字节数 ∈ [1, 512]。
//
// 返回值是业务错误（400 / 10001），handler 可直接透传。
func ValidateWrappedKey(wrappedKey string) error {
	if wrappedKey == "" {
		return nil
	}
	// 标准 base64 解码（严格：拒绝非 base64 字符与错误 padding）。
	raw, err := base64.StdEncoding.DecodeString(wrappedKey)
	if err != nil {
		return pkgerrors.New(pkgerrors.CodeBadRequest,
			"wrapped_key 格式非法（须为标准 base64 编码的信封密钥）")
	}
	if len(raw) > wrappedKeyMaxBytes {
		return pkgerrors.New(pkgerrors.CodeBadRequest,
			fmt.Sprintf("wrapped_key 长度非法（解码后不得超过 %d 字节，实为 %d 字节）",
				wrappedKeyMaxBytes, len(raw)))
	}
	return nil
}

// validateWrappedKeyField 是给请求结构体用的便捷包装：
// 空白串按「未提供」处理（前端表单可能送空白）。
func validateWrappedKeyField(wrappedKey string) error {
	if strings.TrimSpace(wrappedKey) == "" {
		return nil
	}
	return ValidateWrappedKey(wrappedKey)
}
