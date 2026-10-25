package resp

import (
	stderrors "errors"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/google/uuid"
	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/pkg/errors"
)

// Response 全站统一响应 envelope
//
//	{
//	  "code":      业务码 (0=成功),
//	  "message":   业务码默认文案 / 自定义 message,
//	  "data":      业务数据 (omitempty),
//	  "trace_id":  请求链路 ID (omitempty, 用于排错)
//	}
type Response struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
	TraceID string      `json:"trace_id,omitempty"`
}

type PageData struct {
	List     interface{} `json:"list"`
	Total    int64       `json:"total"`
	Page     int         `json:"page"`
	PageSize int         `json:"page_size"`
}

// traceIDHeader trace_id 通过 X-Trace-Id header 透传
const traceIDHeader = "X-Trace-Id"

// maxTraceIDLen 客户端自带 trace_id 的长度上限（超过即视为滥用，重新生成）
const maxTraceIDLen = 64

// ValidTraceID 客户端自带 trace_id 白名单校验：非空、长度 ≤64、
// 字符集 [A-Za-z0-9._-]。原样采信任意值会把超长串回显进响应头并落入
// 访问日志（日志刷量/注入面），不合规一律重新生成。
// 供 pkg/middleware.RequestID 与本包 getOrGenTraceID 共用。
func ValidTraceID(tid string) bool {
	if tid == "" || len(tid) > maxTraceIDLen {
		return false
	}
	for i := 0; i < len(tid); i++ {
		ch := tid[i]
		switch {
		case ch >= '0' && ch <= '9', ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z',
			ch == '-', ch == '_', ch == '.':
		default:
			return false
		}
	}
	return true
}

func getOrGenTraceID(c *app.RequestContext) string {
	tid := string(c.GetHeader(traceIDHeader))
	if !ValidTraceID(tid) {
		tid = uuid.New().String()
	}
	c.Response.Header.Set(traceIDHeader, tid)
	return tid
}

func Success(c *app.RequestContext, data interface{}) {
	c.JSON(consts.StatusOK, Response{
		Code:    errors.CodeSuccess,
		Message: errors.GetMessage(errors.CodeSuccess),
		Data:    data,
		TraceID: getOrGenTraceID(c),
	})
}

func SuccessWithMessage(c *app.RequestContext, message string, data interface{}) {
	c.JSON(consts.StatusOK, Response{
		Code:    errors.CodeSuccess,
		Message: message,
		Data:    data,
		TraceID: getOrGenTraceID(c),
	})
}

// Error 旧版 errors 码的错误响应（兼容老调用方）
func Error(c *app.RequestContext, code int) {
	httpStatus := consts.StatusOK
	if code == errors.CodeUnauthorized {
		httpStatus = consts.StatusUnauthorized
	}
	c.JSON(httpStatus, Response{
		Code:    code,
		Message: errors.GetMessage(code),
		TraceID: getOrGenTraceID(c),
	})
}

func ErrorWithMessage(c *app.RequestContext, code int, message string) {
	httpStatus := consts.StatusOK
	if code == errors.CodeUnauthorized {
		httpStatus = consts.StatusUnauthorized
	}
	c.JSON(httpStatus, Response{
		Code:    code,
		Message: message,
		TraceID: getOrGenTraceID(c),
	})
}

func ErrorWithData(c *app.RequestContext, code int, message string, data interface{}) {
	httpStatus := consts.StatusOK
	if code == errors.CodeUnauthorized {
		httpStatus = consts.StatusUnauthorized
	}
	c.JSON(httpStatus, Response{
		Code:    code,
		Message: message,
		Data:    data,
		TraceID: getOrGenTraceID(c),
	})
}

// NewErrorByCode 新版业务码错误响应（推荐）
// 自动根据业务码选择 HTTP 状态：
//
//	401/403/404/429/500/503 等
func NewErrorByCode(c *app.RequestContext, code int) {
	c.JSON(httpStatusForCode(code), Response{
		Code:    code,
		Message: errcode.Message(code),
		TraceID: getOrGenTraceID(c),
	})
}

// NewErrorWithMessage 新版业务码 + 自定义 message
func NewErrorWithMessage(c *app.RequestContext, code int, message string) {
	c.JSON(httpStatusForCode(code), Response{
		Code:    code,
		Message: message,
		TraceID: getOrGenTraceID(c),
	})
}

// NewErrorWithData 新版业务码 + data
func NewErrorWithData(c *app.RequestContext, code int, message string, data interface{}) {
	c.JSON(httpStatusForCode(code), Response{
		Code:    code,
		Message: message,
		Data:    data,
		TraceID: getOrGenTraceID(c),
	})
}

// httpStatusForCode 业务码 → HTTP 状态码 映射
func httpStatusForCode(code int) int {
	switch {
	case code == errcode.CodeSuccess:
		return consts.StatusOK
	case code == errcode.CodeUnauthorized, code == errcode.CodeTokenInvalid, code == errcode.CodeTokenExpired:
		return consts.StatusUnauthorized
	case code == errcode.CodeForbidden, code == errcode.CodePermissionDeny:
		return consts.StatusForbidden
	case code == errcode.CodeNotFound, code == errcode.CodeShareNotFound, code == errcode.CodeFileNotFound, code == errcode.CodePickupCodeNotFound, code == errcode.CodeUserNotFound:
		return consts.StatusNotFound
	case code == errcode.CodeShareExpired, code == errcode.CodePickupCodeExpired:
		// 「已过期」与「不存在」同语义（404，降低存在性探测面）——与本仓既有
		// 口径一致（各面把「不存在/已过期/被封禁」统一成 404）。
		return consts.StatusNotFound
	case code == errcode.CodeShareReachLimit, code == errcode.CodePickupCodeExhausted:
		// 「取件次数用完」= **拒绝取件**（不是「不存在」）⇒ 403。
		//
		// ⚠️ 2026-10-25 审计 A1：此前 2xxxx 业务码**整族未枚举** ⇒ 一律落
		// `default` ⇒ **HTTP 200**。于是「次数用完」在匿名通道（20007）是 200、
		// 在分享通道（20004，handler 显式写 403）是 403 ⇒ **同一语义事件两个
		// 状态码**；客户端若按 HTTP 状态分支（而非按业务码）就会把「额度用完」
		// 当成成功响应处理。
		//
		// ⇒ 2xxxx 族补齐 HTTP 语义，与分享通道对齐（破坏性变更见发布说明：
		// 客户端应按响应体 `code` 分支，本仓文档亦如此约定）。
		return consts.StatusForbidden
	case code == errcode.CodeRateLimit, code == errcode.CodeTooManyAttempts, code == errcode.CodeAnonymousQuota:
		return consts.StatusTooManyRequests
	case code == errcode.CodeShareBlocked, code == errcode.CodeSharePendingReview,
		code == errcode.CodeUploadDisabled, code == errcode.CodeIPBlocked:
		return consts.StatusForbidden
	case code == errcode.CodeContentRejected:
		return consts.StatusBadRequest
	case code == errcode.CodeDownloadToken, code == errcode.CodeSharePasswordWrong, code == errcode.CodePasswordWrong:
		return consts.StatusUnauthorized
	case code == errcode.CodeMethodNotAllowed:
		return consts.StatusMethodNotAllowed
	case code == errcode.CodeTooLarge:
		return consts.StatusRequestEntityTooLarge
	case code == errcode.CodeUnavailable:
		return consts.StatusServiceUnavailable
	case code == errcode.CodeTimeout:
		return consts.StatusRequestTimeout
	case errcode.IsServerError(code):
		return consts.StatusInternalServerError
	case errcode.IsClientError(code):
		return consts.StatusBadRequest
	default:
		return consts.StatusOK
	}
}

// HTTPStatusForCode 导出业务码 → HTTP 状态 的映射。
//
// 供取件闸门**契约守卫**（bootstrap/share_surface_contract_test.go）核对
// 「契约表登记的 HTTP 状态」与「运行时实际映射」是否一致 ⇒ 2026-10-25 审计 A9
// 指出的「契约表只登记业务码 ⇒ 状态码退化结构上无法被发现」由此闭合。
func HTTPStatusForCode(code int) int {
	return httpStatusForCode(code)
}

// NewTypedError 把实现 ErrCode() int 的 typed error（gate/moderation 等）写成
// 业务错误响应；未实现该接口的错误按内部错误处理。
func NewTypedError(c *app.RequestContext, err error) {
	var coder interface{ ErrCode() int }
	if stderrors.As(err, &coder) {
		NewErrorWithMessage(c, coder.ErrCode(), err.Error())
		return
	}
	NewErrorWithMessage(c, errcode.CodeInternal, err.Error())
}

func Page(c *app.RequestContext, list interface{}, total int64, page, pageSize int) {
	Success(c, PageData{
		List:     list,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}

func BadRequest(c *app.RequestContext, message string) {
	if message == "" {
		message = errors.GetMessage(errors.CodeBadRequest)
	}
	ErrorWithMessage(c, errors.CodeBadRequest, message)
}

func Unauthorized(c *app.RequestContext, message string) {
	if message == "" {
		message = errors.GetMessage(errors.CodeUnauthorized)
	}
	ErrorWithMessage(c, errors.CodeUnauthorized, message)
}

func NotFound(c *app.RequestContext, message string) {
	if message == "" {
		message = errors.GetMessage(errors.CodeNotFound)
	}
	ErrorWithMessage(c, errors.CodeNotFound, message)
}

func InternalError(c *app.RequestContext, message string) {
	if message == "" {
		message = errors.GetMessage(errors.CodeInternalError)
	}
	ErrorWithMessage(c, errors.CodeInternalError, message)
}
