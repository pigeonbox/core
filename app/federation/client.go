package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oekit/kitx/uidgen"
	"github.com/oekit/pkg/httpjson"
	"github.com/pigeonbox/core/pkg/outbound"
)

// 与 p2p 仓（github.com/pigeonbox/p2p）v1 API 的通讯客户端。
//
// 签名负载契约（逐字节，Ed25519 + base64 std；字段以 "\n" 连接，缺省字段
// 空串占位——由 TestPayloadContract 锁定，p2p 侧变动必须双侧同步）：
//
//	注册/心跳:  node_id | url | name | version | caps | ttl_seconds | nonce | ts
//	公告:       node_id | code_hash | expires_at | size_hint | ts
//	撤销公告:   node_id | code_hash | ts
//	注销节点:   node_id | ts
const (
	nodeVersion  = "1"        // 节点协议能力版本
	capsDownload = "download" // M2 联邦取件 = HTTP 直连源节点下载
)

type registryClient struct {
	base  string
	http  *http.Client
	nonce func() (string, error)
}

func newRegistryClient(base string) *registryClient {
	return &registryClient{
		base: base,
		// 5s 原值随收编迁入注册表 TargetFederation 策略（core#87 步骤 1）。
		http:  outbound.New(outbound.TargetFederation),
		nonce: randomNonce,
	}
}

// registryNonceEntropyBytes 注册 nonce 的熵预算：8 字节 = 64 bit 熵 ⇒ 2^64 防
// 碰撞预算（core#86 收编 kitx/uidgen 时定长于此；nonce 只防注册重放，TTL 窗口
// 内不重复即可）。
const registryNonceEntropyBytes = 8

// randSource 包级随机源 seam（默认 crypto/rand.Reader；测试注入必然失败的
// reader 验证错误上抛——core#86：熵源故障绝不能静默发弱 nonce/重复信封 ID）。
// 本文件的 randomNonce 与 invite.go 的 newEnvelopeID 共用同一注入口。
var randSource io.Reader = rand.Reader

// randomNonce 注册/心跳签名负载的随机 nonce。
//
// 随机源失败**显式上抛**（core#86：原实现吞错 ⇒ 熵源故障时静默发全 0 nonce ⇒
// 注册重放防线失守）——调用方 register 收到错误即中止，不发出带弱 nonce 的请求。
func randomNonce() (string, error) {
	return uidgen.RandomHexFrom(randSource, registryNonceEntropyBytes)
}

func buildPayload(parts ...string) []byte {
	return []byte(strings.Join(parts, "\n"))
}

func signPayload(priv ed25519.PrivateKey, payload []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload))
}

// codeHash 口令 → SHA-256 hex。registry 不接触明文（公开确定性哈希；
// 防枚举靠宣布侧熵门槛 + registry 解析限流，见包注释）。
func codeHash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// do 发一次 HTTP+JSON 调用（走 **oekit/pkg/httpjson**：ctx 感知构造请求 →
// 限容读体 → 状态码检查（错误体 rune 安全截断，不腰斩 UTF-8）→ JSON 解码）。
//
// ⚠️ 两处内部行为变化（**均不外泄到 wire**——联邦错误只进日志，handler 侧一律
// 以 available:false 兜住，见 transport/http/handler/federation.go）：
//   - **ctx 真正生效**：此前形参是 `_ context.Context` ⇒ 调用点的 5s/15s 预算
//     只落在 http.Client.Timeout 上，请求 ctx 被丢弃；现在按调用方 ctx 取消。
//   - 状态码判定从「精确等于期望码」放宽为「2xx 即成功」（httpjson 口径）。
//     p2p registry 这几个端点恒回 200 ⇒ 无实际差异。
func (c *registryClient) do(ctx context.Context, method, path string, body any, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = b
	}
	req := httpjson.Request{
		Method: method,
		URL:    c.base + path,
		Body:   payload,
		Header: func(h http.Header) {
			if payload != nil {
				h.Set("Content-Type", "application/json")
			}
		},
	}
	// out 为 nil 时响应体仍需被消费（httpjson 走 Unmarshal，out=nil 会报
	// "json: Unmarshal(nil)"）⇒ 用 RawMessage 承接丢弃。
	target := out
	if target == nil {
		var discard json.RawMessage
		target = &discard
	}
	if err := httpjson.DoJSON(ctx, c.http, req, target); err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	return nil
}

type registerReq struct {
	NodeID     string   `json:"node_id"`
	URL        string   `json:"url"`
	Name       string   `json:"name"`
	Version    string   `json:"version"`
	Caps       []string `json:"caps"`
	TTLSeconds int64    `json:"ttl_seconds"`
	Nonce      string   `json:"nonce"`
	TS         int64    `json:"ts"`
	Sig        string   `json:"sig"`
}

func (c *registryClient) register(ctx context.Context, nodeID string, priv ed25519.PrivateKey, publicURL, name string, ttl time.Duration) error {
	ts := time.Now().Unix()
	ttlSec := int64(ttl / time.Second)
	nonce, err := c.nonce()
	if err != nil {
		return fmt.Errorf("注册 nonce 生成失败（随机源不可用）: %w", err)
	}
	sig := signPayload(priv, buildPayload(nodeID, publicURL, name, nodeVersion, capsDownload,
		strconv.FormatInt(ttlSec, 10), nonce, strconv.FormatInt(ts, 10)))
	return c.do(ctx, http.MethodPost, "/v1/nodes/register", registerReq{
		NodeID: nodeID, URL: publicURL, Name: name, Version: nodeVersion,
		Caps: []string{capsDownload}, TTLSeconds: ttlSec, Nonce: nonce, TS: ts, Sig: sig,
	}, nil)
}

type announceReq struct {
	NodeID    string `json:"node_id"`
	CodeHash  string `json:"code_hash"`
	ExpiresAt int64  `json:"expires_at"`
	SizeHint  int64  `json:"size_hint"`
	TS        int64  `json:"ts"`
	Sig       string `json:"sig"`
}

func (c *registryClient) announce(ctx context.Context, nodeID string, priv ed25519.PrivateKey, hash string, expires time.Time) error {
	ts := time.Now().Unix()
	expiresUnix := expires.Unix()
	sig := signPayload(priv, buildPayload(nodeID, hash, strconv.FormatInt(expiresUnix, 10), "0", strconv.FormatInt(ts, 10)))
	return c.do(ctx, http.MethodPost, "/v1/announces", announceReq{
		NodeID: nodeID, CodeHash: hash, ExpiresAt: expiresUnix, SizeHint: 0, TS: ts, Sig: sig,
	}, nil)
}

type revokeReq struct {
	NodeID string `json:"node_id"`
	TS     int64  `json:"ts"`
	Sig    string `json:"sig"`
}

func (c *registryClient) revoke(ctx context.Context, nodeID string, priv ed25519.PrivateKey, hash string) error {
	ts := time.Now().Unix()
	sig := signPayload(priv, buildPayload(nodeID, hash, strconv.FormatInt(ts, 10)))
	return c.do(ctx, http.MethodDelete, "/v1/announces/"+hash, revokeReq{
		NodeID: nodeID, TS: ts, Sig: sig,
	}, nil)
}

func (c *registryClient) deregister(ctx context.Context, nodeID string, priv ed25519.PrivateKey) error {
	ts := time.Now().Unix()
	sig := signPayload(priv, buildPayload(nodeID, strconv.FormatInt(ts, 10)))
	return c.do(ctx, http.MethodDelete, "/v1/nodes/"+nodeID, revokeReq{
		NodeID: nodeID, TS: ts, Sig: sig,
	}, nil)
}

// resolve 转查 registry。未接入（404）返回 (nil, nil)；其余非 200 视为错误。
func (c *registryClient) resolve(ctx context.Context, code string) (*ResolveInfo, error) {
	var raw struct {
		NodeID    string `json:"node_id"`
		URL       string `json:"url"`
		Name      string `json:"name"`
		ExpiresAt int64  `json:"expires_at"`
		SizeHint  int64  `json:"size_hint"`
	}
	req := httpjson.Request{Method: http.MethodGet, URL: c.base + "/v1/resolve/" + codeHash(code)}
	if err := httpjson.DoJSON(ctx, c.http, req, &raw); err != nil {
		// 404 = 未接入（不是错误）⇒ 调用方按「未命中」处理。
		var se *httpjson.StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("resolve: %w", err)
	}
	return &ResolveInfo{
		NodeID:    raw.NodeID,
		URL:       raw.URL,
		Name:      raw.Name,
		ExpiresAt: time.Unix(raw.ExpiresAt, 0),
		SizeHint:  raw.SizeHint,
	}, nil
}
