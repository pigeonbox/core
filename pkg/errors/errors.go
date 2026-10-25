package errors

import (
	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/pigeonbox/contracts/errcode"
)

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
	CodeSuccess      = errcode.CodeSuccess
	CodeBadRequest   = errcode.CodeInvalidParam
	CodeUnauthorized = errcode.CodeUnauthorized
	CodeForbidden    = errcode.CodeForbidden
	CodeNotFound     = errcode.CodeNotFound
	// CodeConflict：contracts v0.15.1 起直接引用 errcode 常量（v0.22.0 tag 时该常量
	// 未发布曾本地同值，随 deps 仪式切回引用，值不变）。
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

// AppError 是上游 oekit/pkg/errors.BizError 的别名——自研的
// AppError{Code,Message,Cause} 与上游 BizError{BizCode,HTTPCode,Message,Cause}
// 语义同构（业务码 + 描述 + 可选 cause），收编后仓内只留一份实现。
//
// 差异（上游形态为准）：
//   - 字段名 Code → BizCode（上游命名），并新增 HTTPCode（HTTP 状态码，0=由
//     调用方映射；本包构造器不设置，与迁移前一致）；
//   - Error() 文案多出 "biz_code:%d " 前缀（原为纯 message）。
//
// 本仓无任何调用点（AppError 及其构造器全仓仅本文件自身引用，qnap/server/
// openwrt/fnos 四个消费仓均未引用 pkg/errors），故上述差异无对外可观测影响。
type AppError = oekiterrors.BizError

// New 构造一个业务错误（业务码 + 描述）。
func New(code int, message string) *AppError {
	return oekiterrors.NewBizError(code, message)
}

// Wrap 构造一个带原始错误的业务错误。
func Wrap(code int, message string, cause error) *AppError {
	return oekiterrors.WrapBizError(code, message, cause)
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

// IsAppError 判断错误是否为 AppError。
//
// 口径与迁移前逐字一致：仅匹配**直接**的 *AppError（不做 errors.As 链式匹配），
// 故刻意不用上游 IsBizError（后者走 errors.As，命中面更宽）。
func IsAppError(err error) bool {
	_, ok := err.(*AppError)
	return ok
}

// GetErrorCode 取错误的业务码；非 AppError 一律回退 CodeInternalError。
//
// 口径与迁移前逐字一致：仅识别**直接**的 *AppError，故刻意不用上游 AsBizError。
func GetErrorCode(err error) int {
	if appErr, ok := err.(*AppError); ok {
		return appErr.BizCode
	}
	return CodeInternalError
}
