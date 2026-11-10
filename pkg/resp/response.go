package resp

import (
	stderrors "errors"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/google/uuid"
	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/pkg/errors"
	"github.com/pigeonbox/core/pkg/logger"
	"go.uber.org/zap"
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

// 2026-11-06 清理：Error / ErrorWithData / BadRequest / Unauthorized /
// NotFound 五个旧 envelope 帮手在全仓（含测试）**零调用**，已删除。
// 实际错误出口只剩三条：NewErrorWithMessage（业务码+文案）/
// NewErrorByCode（业务码，文案查表）/ NewTypedError（typed error 透传）。
//
// 为什么值得删：这些帮手不带 httpStatusForCode 映射，一律 200 或
// "仅 Unauthorized 特殊对待"——与新口径（业务码→HTTP 状态统一映射）并存时，
// 新代码随手挑到它们就会产出「HTTP 200 里塞错误码」的老形状。

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

// InternalFailureMsg 内部（DB/基建）错误的对外脱敏文案（core#114）。
//
// err 原文**永不**透出——SQL/驱动文本（no such column / database is locked /
// 表名方言）可探测库结构；op 只保留「哪一步失败」的操作语义，可读且无内部细节。
func InternalFailureMsg(op string) string {
	if op == "" {
		return "操作失败，请稍后重试"
	}
	return op + "失败，请稍后重试"
}

// NewInternalError 内部（DB/基建）错误统一出口（core#114）：
//   - err 全文进服务端结构化日志（trace_id 可与响应对账）；
//   - 对外只回脱敏文案（InternalFailureMsg）+ 业务码（自动映射 HTTP 状态）+ trace_id。
//
// 与 gen/handler/share_anonymous 的 sanitizeInternalMsg 同口径；那边是按基建
// 特征（dial tcp/redis/timeout）模式匹配、只覆盖匿名面，本出口是无条件脱敏，
// 用于一切 500 类错误响应（点名参数类错误仍应回可操作文案，走 NewErrorWithMessage）。
func NewInternalError(c *app.RequestContext, code int, op string, err error) {
	logger.Error("internal error", zap.String("op", op), zap.Error(err))
	NewErrorWithMessage(c, code, InternalFailureMsg(op))
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
	// ⚠️ 本 case 已删除（2026-11-06 修重复分支）：同一 switch 里
	// CodeContentRejected 出现过两次，此处（400）在下方 403 之前 ⇒ 下方
	// **永远不可能命中**（Go 按顺序匹配，重复 case 合法但不报错）。
	// 内容审核拒绝语义上是「禁止分享」（与 CodeShareBlocked 同类），统一保留
	// 下方 403 分支；否则「内容违规」会被报成 400 参数错误，前端与运维都会
	// 按「传参有问题」去排查。
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
	case code == errcode.CodeStorageInit, code == errcode.CodeStorageUpload,
		code == errcode.CodeStorageDownload, code == errcode.CodeStorageDelete,
		code == errcode.CodeStorageIO:
		// 存储域的服务端侧故障（初始化/上传/下载/删除/IO 失败）⇒ 500
		return consts.StatusInternalServerError
	case code == errcode.CodeStorageQuota:
		// 存储配额超限（空间满）⇒ 507 Insufficient Storage
		return consts.StatusInsufficientStorage
	case code == errcode.CodeStorageScheme, code == errcode.CodeEndpointDenied:
		// 存储方案不支持 / 端点地址不合法（SSRF 防线）⇒ 400
		return consts.StatusBadRequest
	case code == errcode.CodePresignFailed:
		// 预签名**生成**失败（服务端故障）⇒ 500
		return consts.StatusInternalServerError
	case code == errcode.CodePresignCompleted:
		// 上传会话已完成 ⇒ 409 Conflict（拒绝重放直传）。
		// ⚠️ 此前它与「预签名生成失败」共用 30008 ⇒ 既没有 409 语义，消息也错。
		return consts.StatusConflict
	case code == errcode.CodePresignToken:
		// 预签名 token 无效 ⇒ 403（**刻意不用 401**）。
		// 理由：前端的 axios 拦截器把 **401 当作会话失效** ⇒ 触发 refresh/跳登录；
		// 而 presign 的上传令牌**与会话无关**（匿名直传也会带）⇒ 用 401 会把
		// 「上传凭证无效」误报成「请重新登录」。同族的 share/select 等端点也
		// 遵循「业务语义 401 不许触发刷新」。
		return consts.StatusForbidden
	case code == errcode.CodePresignExpired:
		// 直传凭证已过期 ⇒ 410 Gone（资源曾存在、现不可用）
		return consts.StatusGone
	case code == errcode.CodeFileTypeDenied:
		// 文件类型不允许 ⇒ 415 Unsupported Media Type
		return consts.StatusUnsupportedMediaType
	case code == errcode.CodeContentRejected:
		// 内容未通过审核 ⇒ 403
		return consts.StatusForbidden
	case code == errcode.CodeConflict, code == errcode.CodeUserExists:
		// 资源冲突（10016/40002）⇒ 409。⚠️ 10016 此前未登记（注册面/分片面各自
		// 手写 409、AdminCreateUser 走映射却落 default ⇒ **HTTP 200**）——同一
		// 语义事件两个状态码 + 业务错误进 200（core#115 同族缺陷）。A6b 统一。
		return consts.StatusConflict
	case code == errcode.CodeChunkInvalid:
		// 分片无效（20009）⇒ 422 Unprocessable Entity（前端 multifile.ts 按
		// 「HTTP 422 或 body 20009」判定单片可重试，两个判据之一保持成立）。
		return consts.StatusUnprocessableEntity
	case code == errcode.CodeTotpRequired, code == errcode.CodeTotpInvalid:
		// 二次验证（40012/40013）⇒ 400。⚠️ 不用 401：/user/totp/* 等信任面
		// 收到 401 会被前端拦截器当成会话失效 ⇒ 误触发 refresh/跳登录；登录流
		// 的二验弹框判据也只有 body code 40012（views/user/Login.vue），
		// 且登录面注释本就写明「40012 不返回 401」——统一 400 与两处口径都一致。
		return consts.StatusBadRequest
	case code == errcode.CodeResetTokenBad:
		// 重置链接无效/过期（40014）⇒ 400（客户端可换链接重试，非服务端故障）。
		return consts.StatusBadRequest
	case code == errcode.CodeInternal:
		// 内部错误（10008）⇒ 500。⚠️ 此前未显式枚举 ⇒ 落到下方 IsClientError
		//（10001–10999 也是 1xxxx 段）⇒ **HTTP 400**：「服务端坏了」被报成
		//「参数传错」，监控/网关按 4xx 记健康度、客户端按传参问题排查
		//（core#114 修错误信封统一时暴露的映射缺陷）。
		return consts.StatusInternalServerError
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
//
// pkgerrors.New/Wrap 构造的业务错误（AppError = oekit BizError）**没有**
// ErrCode() 方法——若不单独提取，它们会全部塌成 10008「内部错误」：
// 形状非法这类**客户端**错误被报成服务端故障，调用方无从区分传参与
// 服务端坏了（2026-11-02 core#30 S27 冒烟实测暴露：wrapped_key 形状
// 校验的 10001 到 wire 上成了 10008）。⇒ 这里按 BizCode 透传。
func NewTypedError(c *app.RequestContext, err error) {
	var coder interface{ ErrCode() int }
	if stderrors.As(err, &coder) {
		NewErrorWithMessage(c, coder.ErrCode(), err.Error())
		return
	}
	var biz *errors.AppError
	if stderrors.As(err, &biz) {
		// 用 Message 而非 Error()：后者带 "biz_code:%d " 前缀（日志语义），
		// 不该进面向用户/调用方的 message 字段。
		NewErrorWithMessage(c, biz.BizCode, biz.Message)
		return
	}
	// 兜底也是内部错误 ⇒ 同样脱敏（core#114 A6b：此前这里裸回 err.Error()，
	// 未实现 ErrCode 的错误把 SQL/驱动文本经 10008 出网——与 NewInternalError
	// 的口径对齐：细节进日志，对外只留脱敏文案）。
	NewInternalError(c, errcode.CodeInternal, "", err)
}

func Page(c *app.RequestContext, list interface{}, total int64, page, pageSize int) {
	Success(c, PageData{
		List:     list,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}

func InternalError(c *app.RequestContext, message string) {
	if message == "" {
		message = errors.GetMessage(errors.CodeInternalError)
	}
	ErrorWithMessage(c, errors.CodeInternalError, message)
}
