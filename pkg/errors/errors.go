package errors

import "github.com/pigeonbox/contracts/errcode"

// 统一错误码（2026-10-15）：legacy 1001-1008 退役，常量名保留为 errcode
// 别名重钉到 contracts/errcode 对应值——全部经由常量的发射点一次性迁移，
// 无需改任何调用方。wire 上旧值不再出现（破坏性变更，随主/次版本公告，
// 对照表见文档站 error-codes 页）：
//
//	1001 → 10001 InvalidParam
//	1002 → 10002 Unauthorized
//	1003 → 10003 Forbidden
//	1004 → 10004 NotFound
//	1005 → 10016 Conflict（errcode 新增）
//	1006 → 10008 Internal
//	1007 → 10006 Unavailable
//	1008 → 10005 RateLimit
const (
	CodeSuccess            = errcode.CodeSuccess
	CodeBadRequest         = errcode.CodeInvalidParam
	CodeUnauthorized       = errcode.CodeUnauthorized
	CodeForbidden          = errcode.CodeForbidden
	CodeNotFound           = errcode.CodeNotFound
	// CodeConflict=10016：contracts v0.15.1 起可直接引 errcode.CodeConflict（v0.22.0 tag 时
	// 该常量未发布故本地同值，此处随 deps 仪式切回引用，值不变）。
	// 侧该常量随下一版 tag 发布，届时改回 errcode.CodeConflict 引用（跨仓
	// 钉版须先 tag，go.mod 现钉 v0.15.0 不含此常量）。
	CodeConflict           = errcode.CodeConflict
	CodeInternalError      = errcode.CodeInternal
	CodeServiceUnavailable = errcode.CodeUnavailable
	CodeTooManyRequests    = errcode.CodeRateLimit
)

// codeMessages 与 errcode 默认文案对齐（调用方传空 message 时的兜底）；
// 常量即 errcode 值，故此处键值与 contracts 侧文案保持一致。
var codeMessages = map[int]string{
	CodeSuccess:            "success",
	CodeBadRequest:         "invalid parameter",
	CodeUnauthorized:       "unauthorized",
	CodeForbidden:          "forbidden",
	CodeNotFound:           "not found",
	CodeConflict:           "resource conflict",
	CodeInternalError:      "internal server error",
	CodeServiceUnavailable: "service unavailable",
	CodeTooManyRequests:    "too many requests",
}

type AppError struct {
	Code    int
	Message string
	Cause   error
}

func (e *AppError) Error() string {
	if e.Cause != nil {
		return e.Message + ": " + e.Cause.Error()
	}
	return e.Message
}

func (e *AppError) Unwrap() error {
	return e.Cause
}

func New(code int, message string) *AppError {
	return &AppError{Code: code, Message: message}
}

func Wrap(code int, message string, cause error) *AppError {
	return &AppError{Code: code, Message: message, Cause: cause}
}

func GetMessage(code int) string {
	if msg, ok := codeMessages[code]; ok {
		return msg
	}
	return "unknown error"
}

func BadRequest(message string) *AppError {
	if message == "" {
		message = GetMessage(CodeBadRequest)
	}
	return New(CodeBadRequest, message)
}

func Unauthorized(message string) *AppError {
	if message == "" {
		message = GetMessage(CodeUnauthorized)
	}
	return New(CodeUnauthorized, message)
}

func Forbidden(message string) *AppError {
	if message == "" {
		message = GetMessage(CodeForbidden)
	}
	return New(CodeForbidden, message)
}

func NotFound(message string) *AppError {
	if message == "" {
		message = GetMessage(CodeNotFound)
	}
	return New(CodeNotFound, message)
}

func Conflict(message string) *AppError {
	if message == "" {
		message = GetMessage(CodeConflict)
	}
	return New(CodeConflict, message)
}

func InternalError(message string) *AppError {
	if message == "" {
		message = GetMessage(CodeInternalError)
	}
	return New(CodeInternalError, message)
}

func IsAppError(err error) bool {
	_, ok := err.(*AppError)
	return ok
}

func GetErrorCode(err error) int {
	if appErr, ok := err.(*AppError); ok {
		return appErr.Code
	}
	return CodeInternalError
}
