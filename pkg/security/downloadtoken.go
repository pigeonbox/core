// Package security 提供独立的安全原语：
//   - DownloadToken: 取件下载令牌（时间窗 HMAC + 恒时比较）
//   - SSRF: 存储端点 URL 校验（scheme 白名单 + 私网地址策略）
package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// defaultDownloadTokenTTL 默认令牌有效期（对齐上游 ~1000s 窗口）。
//
// 真实值可由 security.download_token.validity_seconds 覆盖
// （2026-10-24：此前配置项存在但**从未被读取**，运维改配置无效 —— 硬编码常量）。
const defaultDownloadTokenTTL = 1000 * time.Second

// downloadTokenSecret 令牌签名密钥（bootstrap 注入，默认复用 jwt_secret）。
var downloadTokenSecret []byte

// downloadTokenTTLSec 令牌有效期（秒），可由配置注入。
var downloadTokenTTLSec atomic.Int64

func init() {
	downloadTokenTTLSec.Store(int64(defaultDownloadTokenTTL / time.Second))
}

// SetDownloadTokenSecret 注入下载令牌签名密钥（应与 jwt_secret 等强的独立随机值）。
func SetDownloadTokenSecret(secret string) {
	if secret == "" {
		return
	}
	downloadTokenSecret = []byte(secret)
}

// SetDownloadTokenTTL 设置令牌有效期（秒）；<=0 时回落到默认值。
func SetDownloadTokenTTL(seconds int) {
	if seconds <= 0 {
		seconds = int(defaultDownloadTokenTTL / time.Second)
	}
	downloadTokenTTLSec.Store(int64(seconds))
}

// DownloadTokenTTL 当前令牌有效期。
func DownloadTokenTTL() time.Duration {
	return time.Duration(downloadTokenTTLSec.Load()) * time.Second
}

// tokenPayload 令牌消息 = code + "." + 过期 Unix 秒 + "." + nonce。
//
// nonce（2026-10-24 新增）：此前载荷是 `code.exp`，签名只随秒级 exp 变化 ⇒
// **同一秒内签发的令牌逐字节相同**。而计次键取令牌指纹（见 DownloadTokenFingerprint）
// ⇒ 同秒签发的两个令牌会被当成「同一次取件」⇒ 第二个取件人不扣次数（额度漏扣）。
// 加上随机 nonce 后每个令牌唯一 ⇒ 一个令牌 = 一次取件。
func tokenPayload(code string, exp int64, nonce string) string {
	return code + "." + strconv.FormatInt(exp, 10) + "." + nonce
}

// randomNonce 生成 16 字节随机 nonce（hex）。
func randomNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败属于系统级异常；退化为时间戳+计数器拼接的弱 nonce
		// 仍能保证「同一秒内不同」——计次去重只需要区分度，不需要密码学强度。
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

func sign(payload string) string {
	mac := hmac.New(sha256.New, downloadTokenSecret)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// GenerateDownloadToken 生成下载令牌 "exp.nonce.sig"。
//
// 令牌 = **一次取件的凭证**：下载时按令牌计次（同一令牌至多扣 1 次），
// 见 repo/db/model.PickupCharge。空密钥（未注入）时返回空串——调用方按
// 「功能未启用」处理。
//
// ⚠️ 形态变更（2026-10-24，破坏性）：此前是 "exp.sig"（载荷 `code.exp`），
// 现在是 "exp.nonce.sig"（载荷 `code.exp.nonce`）。旧令牌在升级后校验失败
// （窗口 ≤ TTL，取件页重新取一次即可）。
func GenerateDownloadToken(code string) string {
	if len(downloadTokenSecret) == 0 {
		return ""
	}
	exp := time.Now().Add(DownloadTokenTTL()).Unix()
	nonce := randomNonce()
	return fmt.Sprintf("%d.%s.%s", exp, nonce, sign(tokenPayload(code, exp, nonce)))
}

// VerifyDownloadToken 恒时比较校验令牌（exp 窗口 + HMAC 签名）。
func VerifyDownloadToken(code, token string) bool {
	return VerifyDownloadTokenFingerprint(code, token) != ""
}

// VerifyDownloadTokenFingerprint 校验令牌并返回其**计次指纹**（空串 = 无效）。
//
// 令牌串 = "exp.nonce.sig"，签名载荷 = "code.exp.nonce" ⇒ 签名覆盖分享码
// （令牌不可跨分享使用），nonce 落在令牌串里（可重建载荷，无需服务端记账）。
//
// 指纹 = HMAC(secret, "fp|" + 令牌)：单向，**不是令牌本身** —— 计次记录要落库，
// 若直接存签名段，DB 泄漏即等于泄漏可直接下载的凭证。
func VerifyDownloadTokenFingerprint(code, token string) string {
	if len(downloadTokenSecret) == 0 {
		return ""
	}
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return ""
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return ""
	}
	expected := sign(tokenPayload(code, exp, parts[1]))
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return ""
	}
	return tokenFingerprint(token)
}

// tokenFingerprint 计算令牌的单向计次指纹（与有效性校验分离，便于单独复用）。
func tokenFingerprint(token string) string {
	mac := hmac.New(sha256.New, downloadTokenSecret)
	mac.Write([]byte("fp|" + token))
	return hex.EncodeToString(mac.Sum(nil))
}

// TextViewKey **文本交付面**的计次键：按 (分享码, 取件人 IP) 派生。
//
// 为什么文本面不用令牌指纹（2026-10-24 回归修复）：select 每次都会签发**新**令牌
// ⇒ 令牌指纹每次都不同 ⇒ 收件人**刷新一次页面就扣一次**取件次数 ⇒ 额度很快耗尽，
// 第三次 select 直接 403「超过取件次数」——**正文都看不了**（改动前 select 从不
// 扣次，文本分享可以随便刷新查看 ⇒ 这是引入的功能性回归）。
//
// 文本的交付动作是「打开页面」而非「下载」⇒ 判重粒度应当是**取件者**而不是
// 一次请求：同一 IP 重复打开/刷新 = 同一次取件（只扣 1 次，能一直看）；
// 不同的人（不同 IP）= 各自的取件（各扣 1 次 ⇒ owner 的「N 次」= N 个取件者）。
//
// 单向（HMAC）⇒ 落库的不是明文 IP。与 DownloadSessionKey 的域不同（"text|" vs
// "ip|"）⇒ 两类键不会互相撞。
func TextViewKey(code, clientIP string) string {
	if len(downloadTokenSecret) == 0 || clientIP == "" {
		return ""
	}
	mac := hmac.New(sha256.New, downloadTokenSecret)
	mac.Write([]byte("text|" + code + "|" + clientIP))
	return hex.EncodeToString(mac.Sum(nil))
}

// DownloadSessionKey 无令牌部署时的兜底「取件会话」键（按分享码 + 取件人 IP）。
//
// 场景：`security.download_token.enabled=false` ⇒ 没有令牌 ⇒ 没有天然的
// 「一次取件」凭证 ⇒ 若每次交付都算一次取件，多文件分享逐个下载会逐个扣次
// （「取件次数=1 只能下载第一个文件」的老问题在关闭令牌时重现）。
// ⇒ 用 (分享码, 取件人 IP) 兜底：同一收件人的一次取件（多文件/续传/重下）
// 只扣 1 次；不同分享互不干扰。
//
// 单向（HMAC）⇒ 落库的不是明文 IP。域不同（"ip|" vs "fp|"）⇒ 不会与令牌指纹撞键。
func DownloadSessionKey(code, clientIP string) string {
	if len(downloadTokenSecret) == 0 || clientIP == "" {
		return ""
	}
	mac := hmac.New(sha256.New, downloadTokenSecret)
	mac.Write([]byte("ip|" + code + "|" + clientIP))
	return hex.EncodeToString(mac.Sum(nil))
}

// DownloadTokenFingerprint 令牌的计次指纹（**不校验有效性**）。
//
// 签发方用它在签发的同一刻算计次键（文本分享在 select 交付正文时计次）；
// 校验方用 VerifyDownloadTokenFingerprint（校验 + 指纹一体）。
func DownloadTokenFingerprint(token string) string {
	if len(downloadTokenSecret) == 0 || token == "" {
		return ""
	}
	return tokenFingerprint(token)
}
