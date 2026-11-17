// unavailable.go —— 「服务暂时不可用」的 typed error 词汇表（可恢复的基建故障）。
//
// 与 shareblocked.go 同款定位：跨域共用的 typed error 收口到本包，避免各域
// 各持一份逐字相同的拷贝（历史漂移见 shareblocked.go 注释）。
//
// 为什么需要它：基建故障（Redis 宕机 / 存储后端不可达）在 wire 上必须与
// 「参数非法」**可区分**——客户端据此回退到别的上传通道（或稍后重试），而不是
// 当成自己的参数错误反复重试。此前这类故障以裸 error 上抛 ⇒ handler 兜底
// NewInternalError ⇒ HTTP 500 + 笼统文案，前端无从分辨。
//
// 本类型实现 app 侧 typed error 约定（ErrCode() int）⇒ handler 侧
// resp.NewTypedError 按业务码透传 ⇒ errcode.CodeUnavailable（10006）在
// pkg/resp/response.go 的映射里是 **HTTP 503**（不是 500）。
package errors

import "github.com/pigeonbox/contracts/errcode"

// ServiceUnavailableError 服务暂时不可用（可恢复的基建故障，如会话存储宕机）。
//
// Error() 只返回**面向调用方的可操作文案**：原始故障文本（dial tcp / connect
// refused / 驱动报错）会经 handler 的 NewTypedError 直接进响应体，不得外泄。
// 底层原因用 Unwrap 保留在错误链里，供服务端日志与 errors.Is/As 判据使用。
type ServiceUnavailableError struct {
	Msg   string
	Cause error
}

func (e *ServiceUnavailableError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return GetMessage(CodeServiceUnavailable)
}

// ErrCode 落「服务不可用」（10006 ⇒ HTTP 503），与「内部错误」/「参数错误」区分。
func (e *ServiceUnavailableError) ErrCode() int { return errcode.CodeUnavailable }

// Unwrap 保留底层原因：错误链仍可追到原始故障（排障信息不丢）。
func (e *ServiceUnavailableError) Unwrap() error { return e.Cause }

// NewServiceUnavailable 构造「服务暂时不可用」错误。
//
// msg 为面向调用方的文案；cause 为底层原因（可为 nil）。
func NewServiceUnavailable(msg string, cause error) *ServiceUnavailableError {
	return &ServiceUnavailableError{Msg: msg, Cause: cause}
}
