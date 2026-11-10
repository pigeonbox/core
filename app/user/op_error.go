package user

import "github.com/pigeonbox/contracts/errcode"

// OpError 业务可读错误（文案面向用户/集成方 + 业务码供 handler 映射 wire）。
//
// 为什么需要它（core#114 A6b）：handler 的服务调用 catch-all 此前只能
// 「一律 err.Error() 出网」（SQL/驱动文本泄漏）或「一律脱敏」（把
// 「旧密码错误」这类可操作文案也抹掉）。有了 ErrCode，出口可以机械三分：
// typed（含本类型）按业务码透传保留文案；其余按内部错误脱敏进日志。
//
// Error() 保持**纯文案**（不带 biz_code 前缀）——与既有
// errors.New("旧密码错误") 等返回值的 err.Error() 逐字一致，零 wire 文案变化。
type OpError struct {
	Code int
	Msg  string
}

func (e *OpError) Error() string { return e.Msg }
func (e *OpError) ErrCode() int  { return e.Code }

// errParam 便捷构造：参数/业务类拒绝（wire 口径 400 + 10001，core#102/#104）。
func errParam(msg string) *OpError { return &OpError{Code: errcode.CodeInvalidParam, Msg: msg} }

// errNotFound 便捷构造：客户端点名的资源不存在/已失效（wire 口径 404 + 10004）。
func errNotFound(msg string) *OpError { return &OpError{Code: errcode.CodeNotFound, Msg: msg} }
