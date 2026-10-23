// Package security 提供独立的安全原语：
//   - DownloadToken: 取件下载令牌（时间窗 HMAC + 恒时比较）
//   - SSRF: 存储端点 URL 校验（scheme 白名单 + 私网地址策略）
package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// downloadTokenTTL 默认令牌有效期（对齐上游 ~1000s 窗口）
const downloadTokenTTL = 1000 * time.Second

// ErrDownloadToken 无效或过期的下载令牌
var ErrDownloadToken = errors.New("下载令牌无效或已过期，请重新获取取件信息")

// downloadTokenSecret 令牌签名密钥（bootstrap 注入，默认复用 jwt_secret）
var downloadTokenSecret []byte

// SetDownloadTokenSecret 注入下载令牌签名密钥（应与 jwt_secret 等强的独立随机值）。
func SetDownloadTokenSecret(secret string) {
	if secret == "" {
		return
	}
	downloadTokenSecret = []byte(secret)
}

// tokenPayload 令牌消息 = code + "." + 过期 Unix 秒（+ 可选口径标记）。
//
// 「已扣次」口径标记（paid）进**签名载荷** ⇒ 无法伪造（HMAC 覆盖整个载荷）。
func tokenPayload(code string, exp int64, paid bool) string {
	p := code + "." + strconv.FormatInt(exp, 10)
	if paid {
		p += ".paid"
	}
	return p
}

func sign(payload string) string {
	mac := hmac.New(sha256.New, downloadTokenSecret)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// TokenFlavor 下载令牌的校验结果与**扣次口径**。
type TokenFlavor int

const (
	// TokenInvalid 令牌无效/过期/签名不符
	TokenInvalid TokenFlavor = iota
	// TokenNormal 普通令牌：签发方**未**扣过取件次数 ⇒ 下载时扣 1 次
	// （select 面签发；匿名 download 跳转面签发——Peek 不扣次）
	TokenNormal
	// TokenPaid 「已扣次」令牌：签发方**已**扣过取件次数（匿名取件 retrieve 面签发）
	// ⇒ 下载时**不再**扣次（否则一次取件扣两次，count 样式下对方次数减半）
	TokenPaid
)

// GenerateDownloadToken 生成下载令牌 "exp.sig"（普通口径：下载时扣取件次数）。
// 空密钥（未注入）时返回空串——调用方按"功能未启用"处理。
func GenerateDownloadToken(code string) string {
	return generateDownloadToken(code, false)
}

// GeneratePaidDownloadToken 生成「已扣次」下载令牌（匿名取件通道专用）。
//
// 背景（2026-10-24 真机实测）：匿名取件 retrieve 自身**扣了取件次数**并记了取件人，
// 随它下发的 download_url 再走 /share/download 时又扣一次 ⇒ 一次取件扣 2 次、
// 记 2 个取件人（count 过期样式下对方可用次数直接减半）。而分享域的 8 位码路径是
// select(不扣) + download(扣 1) ⇒ 两条通道口径分叉。⇒ 匿名取件签**已扣次**令牌，
// 下载端见此口味即不再扣次、不再重复记取件人 ⇒ 一次取件 = 扣 1 次 + 记 1 个取件人
// （与 API 文档承诺的匿名通道「同扣次数」一致）。
func GeneratePaidDownloadToken(code string) string {
	return generateDownloadToken(code, true)
}

func generateDownloadToken(code string, paid bool) string {
	if len(downloadTokenSecret) == 0 {
		return ""
	}
	exp := time.Now().Add(downloadTokenTTL).Unix()
	return fmt.Sprintf("%d.%s", exp, sign(tokenPayload(code, exp, paid)))
}

// VerifyDownloadToken 恒时比较校验令牌（exp 窗口 + HMAC 签名）。
// **两种口径都算有效**（普通/已扣次）——有效性判定与扣次口径是两件事。
func VerifyDownloadToken(code, token string) bool {
	return VerifyDownloadTokenFlavor(code, token) != TokenInvalid
}

// VerifyDownloadTokenFlavor 校验令牌并返回其扣次口径（恒时比较）。
func VerifyDownloadTokenFlavor(code, token string) TokenFlavor {
	if len(downloadTokenSecret) == 0 {
		return TokenInvalid
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return TokenInvalid
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return TokenInvalid
	}
	// 两种口径都验一遍（恒时比较，避免用响应时间区分口径）
	normal := sign(tokenPayload(code, exp, false))
	paid := sign(tokenPayload(code, exp, true))
	got := parts[1]
	switch {
	case hmac.Equal([]byte(normal), []byte(got)):
		return TokenNormal
	case hmac.Equal([]byte(paid), []byte(got)):
		return TokenPaid
	default:
		return TokenInvalid
	}
}

