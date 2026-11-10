package utils

import (
	"errors"
	"fmt"
	"net/mail"
)

// maxEmailLen 邮箱地址最大长度（RFC 5321 实用上限：local ≤64 + @ + domain ≤255
// 的交集惯例 254）。
const maxEmailLen = 254

// ValidateEmail 邮箱格式收口（core#119）。
//
// 邮箱最终进 SMTP 信封头（notify.buildMessage 的 To/From 按字符串拼接），
// 输入层必须拒绝 CRLF 等控制字符——否则就是 SMTP 头注入面（Bcc 外带通知
// 正文 / 注入附加头）。对照：文件名有 SanitizeFileName、回显头有
// sanitizeHeaderValue，邮箱这条链此前零校验。
//
// 判据三件套（缺一不可）：
//  1. 拒绝控制字符（含 \r \n \t \x00）与超长输入；
//  2. net/mail.ParseAddress 可解析；
//  3. 地址字面量与输入**全等**——拒绝 display-name 形态（"Name <a@b.com>"）、
//     解析被归一化的形态（引号本地部等，解析结果 ≠ 原文）、首尾空白。
//
// 空邮箱在这里算非法；业务上「可以不填」的入口（如管理端建号）由调用方
// 自行放行空串，再对非空值调用本函数。
func ValidateEmail(email string) error {
	if email == "" {
		return errors.New("邮箱不能为空")
	}
	if len(email) > maxEmailLen {
		return fmt.Errorf("邮箱长度不得超过 %d 个字符", maxEmailLen)
	}
	for _, r := range email {
		if r < 0x20 || r == 0x7f {
			return errors.New("邮箱含有非法控制字符")
		}
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return errors.New("邮箱格式不正确")
	}
	return nil
}
