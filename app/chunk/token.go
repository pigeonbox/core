// token.go 分片会话令牌与归属校验的单一实现。
//
// 此前 transport/handler（multi-bind）与 gen/handler/chunk 各持一份逐字孪生
// （chunkSessionToken/verifyChunkToken/ownedByCaller，注释自认"同源同语义"），
// 收口到本域导出函数后两处只留薄委托。
package chunk

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/authz"
	"github.com/pigeonbox/core/repo/db/model"
)

// SessionToken 分片会话令牌：HMAC("chunk-session:"+uploadID, key)。客户端从
// init 响应头 X-Upload-Token 取得并在后续请求回传，即可在 IP 漂移（移动网络/
// CGNAT）后仍通过归属校验。密钥复用 presign 签名密钥（conf.PresignSigningKey
// 单源：PB_PRESIGN_SIGNING_KEY 优先，缺省回退 jwt_secret）；无密钥返回空串
// （令牌通道关闭，仅 IP/用户匹配生效）。
func SessionToken(uploadID string) string {
	key := conf.PresignSigningKey()
	if key == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte("chunk-session:" + uploadID))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySessionToken 客户端回传的会话令牌是否有效。
func VerifySessionToken(uploadID, token string) bool {
	want := SessionToken(uploadID)
	return want != "" && token != "" && hmac.Equal([]byte(want), []byte(token))
}

// OwnedByCaller 分片会话归属校验（治理）：控制记录记有 OwnerIP 时，要求
// IP 一致、同一登录用户或持有效会话令牌；老数据（OwnerIP 为空）此前无条件
// 放行——同网任意调用方可向他人进行中会话写分片/取消/完成（2026-10-08 加固
// 收紧为 fail-closed：仅令牌或同用户可通过，匿名老会话不可恢复，分片会话是
// 短生命周期数据，升级窗口外无存量）。
// clientIP/callerUserID 由调用方按各路由的身份来源提取后传入（JWT/匿名、
// ctx 值/c.Get 两种路径判定口径一致）。
func OwnedByCaller(info *model.UploadChunk, clientIP string, sessionToken string, callerUserID *uint) bool {
	if info.OwnerIP == "" {
		return VerifySessionToken(info.UploadID, sessionToken) ||
			authz.OwnedByUser(info.UserID, callerUserID)
	}
	if authz.OwnedByIP(info.OwnerIP, clientIP) {
		return true
	}
	if VerifySessionToken(info.UploadID, sessionToken) {
		return true
	}
	if authz.OwnedByUser(info.UserID, callerUserID) {
		return true
	}
	return false
}

// chunkSessionOwnedBy init 阶段的会话归属判定（与 OwnedByCaller 同语义，
// 入参来自 InitiateUploadReq 而非 RequestContext；OwnerIP 为空的老数据
// 视为无主空会话，走调用方的孤儿回收分支）。
func chunkSessionOwnedBy(info *model.UploadChunk, ownerIP string, userID *uint, sessionToken string) bool {
	if authz.OwnedByIP(info.OwnerIP, ownerIP) {
		return true
	}
	if VerifySessionToken(info.UploadID, sessionToken) {
		return true
	}
	if authz.OwnedByUser(info.UserID, userID) {
		return true
	}
	return false
}
