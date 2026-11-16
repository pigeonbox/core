package admin

// WebAuthn 软认证器（测试专用）：按规范拼装 attestation/assertion 应答，
// 与服务端 challenge/origin/rpId 现场对齐 —— 端到端正负例都靠它驱动。
//
// CBOR/COSE 编码一律委托库（webauthncbor/webauthncose），本文件**不手写**
// 协议编解码；只做规范定义的字节拼接（authData 布局）与 ECDSA 签名。

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/stretchr/testify/require"
)

// virtualAuthenticator 测试用软认证器：一把 EC P-256 凭证 + 单调计数器。
type virtualAuthenticator struct {
	key     *ecdsa.PrivateKey
	credID  []byte
	counter uint32
}

func newVirtualAuthenticator(t *testing.T) *virtualAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	credID := make([]byte, 32)
	_, err = rand.Read(credID)
	require.NoError(t, err)
	return &virtualAuthenticator{key: key, credID: credID}
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func (v *virtualAuthenticator) clientData(t *testing.T, typ, challenge, origin string) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"type": typ, "challenge": challenge, "origin": origin, "crossOrigin": false,
	})
}

// createResponse 生成 navigator.credentials.create 应答（attestation fmt=none）。
func (v *virtualAuthenticator) createResponse(t *testing.T, challenge, rpID, origin string) []byte {
	t.Helper()
	cd := v.clientData(t, "webauthn.create", challenge, origin)
	// SEC1 未压缩点：0x04 || X(32) || Y(32)——Go 1.26 起 pub.X/Y 已弃用，
	// 坐标改由 PublicKey.Bytes() 取
	rawPub, err := v.key.PublicKey.Bytes()
	require.NoError(t, err)
	require.Len(t, rawPub, 65, "P-256 未压缩点必须 65 字节")
	coseKey, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{KeyType: 2, Algorithm: -7}, // EC2 / ES256
		Curve:         1,                                                     // P-256
		XCoord:        rawPub[1:33],
		YCoord:        rawPub[33:65],
	})
	require.NoError(t, err)

	rpHash := sha256.Sum256([]byte(rpID))
	var ad bytes.Buffer
	ad.Write(rpHash[:])
	ad.WriteByte(0x41) // UP | AT
	v.counter++
	require.NoError(t, binary.Write(&ad, binary.BigEndian, v.counter))
	ad.Write(make([]byte, 16)) // AAGUID 全零（fmt=none 无 attestation）
	require.NoError(t, binary.Write(&ad, binary.BigEndian, uint16(len(v.credID))))
	ad.Write(v.credID)
	ad.Write(coseKey)

	attObj, err := webauthncbor.Marshal(map[string]any{
		"fmt": "none", "attStmt": map[string]any{}, "authData": ad.Bytes(),
	})
	require.NoError(t, err)
	return mustJSON(t, map[string]any{
		"id": b64url(v.credID), "rawId": b64url(v.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64url(cd),
			"attestationObject": b64url(attObj),
			"transports":        []string{"internal"},
		},
		"clientExtensionResults": map[string]any{},
	})
}

// getResponse 生成 navigator.credentials.get 应答（断言）。
func (v *virtualAuthenticator) getResponse(t *testing.T, challenge, rpID, origin string, userHandle []byte) []byte {
	t.Helper()
	cd := v.clientData(t, "webauthn.get", challenge, origin)
	rpHash := sha256.Sum256([]byte(rpID))
	var ad bytes.Buffer
	ad.Write(rpHash[:])
	ad.WriteByte(0x05) // UP | UV
	v.counter++
	require.NoError(t, binary.Write(&ad, binary.BigEndian, v.counter))

	clientHash := sha256.Sum256(cd)
	signed := append(append([]byte{}, ad.Bytes()...), clientHash[:]...)
	sigHash := sha256.Sum256(signed)
	sig, err := ecdsa.SignASN1(rand.Reader, v.key, sigHash[:])
	require.NoError(t, err)

	resp := map[string]any{
		"clientDataJSON":    b64url(cd),
		"authenticatorData": b64url(ad.Bytes()),
		"signature":         b64url(sig),
	}
	if userHandle != nil {
		resp["userHandle"] = b64url(userHandle)
	}
	return mustJSON(t, map[string]any{
		"id": b64url(v.credID), "rawId": b64url(v.credID), "type": "public-key",
		"response": resp,
	})
}

// mutateCredential 对已生成应答做定点篡改（负例用）。
func mutateCredential(t *testing.T, body []byte, fn func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	fn(m)
	return mustJSON(t, m)
}

// tamperSignature 翻转签名最后一个字节（正例的签名必然失效）。
func tamperSignature(m map[string]any) {
	resp := m["response"].(map[string]any)
	sig, err := base64.RawURLEncoding.DecodeString(resp["signature"].(string))
	if err != nil || len(sig) == 0 {
		panic("signature 不是 base64url")
	}
	sig[len(sig)-1] ^= 0xff
	resp["signature"] = b64url(sig)
}

// tamperAttestationObject 破坏 attestationObject 首字节（CBOR 解析必炸）。
func tamperAttestationObject(m map[string]any) {
	resp := m["response"].(map[string]any)
	raw, err := base64.RawURLEncoding.DecodeString(resp["attestationObject"].(string))
	if err != nil || len(raw) == 0 {
		panic("attestationObject 不是 base64url")
	}
	raw[0] ^= 0xff
	resp["attestationObject"] = b64url(raw)
}
