package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/pigeonbox/core/pkg/logger"
)

// serverName MCP server 标识（initialize 的 serverInfo.name）。
const serverName = "pigeonbox"

// methodListTools 工具列表方法名（SDK 内部常量为非导出，按协议字面量匹配）。
const methodListTools = "tools/list"

// callerCtxKey 调用者身份在 context 中的键。
type callerCtxKey struct{}

// WithCaller 把调用者身份注入 context。
//
// transport 层（Hertz handler）在进 net/http 桥接**之前**注入：官方 SDK 把
// http.Request 的 ctx 一路透传到工具 handler（SDK 源码注释："Pass
// req.Context() here, to allow middleware to add context values"），
// 因此工具层与中间件都能读到同一份身份。
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerCtxKey{}, c)
}

// CallerFromContext 读取调用者身份（缺失=游客零值）。
func CallerFromContext(ctx context.Context) Caller {
	c, _ := ctx.Value(callerCtxKey{}).(Caller)
	return c
}

// newMCPServer 构建官方 SDK server。
//
// 协议版本协商交给 SDK：未收窄 SupportedProtocolVersions 时默认支持全部已知
// 版本（最新 2026-07-28；握手期 2025-11-25 / 2025-06-18 / 2025-03-26 向下
// 兼容），客户端报什么版本就协商什么版本，老客户端无须改造。
//
// 工具注册走 SDK 的类型化入口（入参/出参结构体 → 推断 JSON Schema 2020-12
// + 严格入参校验 + structuredContent 出参），注册表见 tools.go。
func (s *Service) newMCPServer() *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{
		Name:    serverName,
		Version: s.version,
	}, &sdk.ServerOptions{
		Instructions: "PigeonBox 文件快递柜：创建/查询分享、寄件码、预览、联邦与运维工具。",
		Logger:       mcpSlogLogger(),
	})

	// 作用域过滤：tools/list 按调用者身份裁剪可见工具（列表过滤不是授权
	// 边界，tools/call 在工具层复核）。
	srv.AddReceivingMiddleware(s.scopeFilterMiddleware)

	for _, spec := range toolSpecs {
		spec.Register(s, srv)
	}
	return srv
}

// toolGuard 作用域闸门 + 类型化入参/出参桥接。
//
// 作用域复核放在这里（tools/list 过滤不是授权边界）；业务失败一律以 error
// 返回 —— SDK 会包成**工具执行错误**（result.isError=true），模型可据此自纠
// （MCP SEP-1303）；返回的 Out 由 SDK 校验并写入 structuredContent，同时以
// JSON 文本块回填 content（老客户端亦可读）。
func toolGuard[In, Out any](minScope int, fn func(ctx context.Context, in In, caller Caller) (Out, error)) sdk.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
		caller := CallerFromContext(ctx)
		if minScope > caller.scopeLevel() {
			var zero Out
			return nil, zero, fmt.Errorf("该工具不可用（需要更高身份：%s）", scopeLabel(minScope))
		}
		out, err := fn(ctx, in, caller)
		if err != nil {
			var zero Out
			return nil, zero, err
		}
		return nil, out, nil
	}
}

// toolSpecByName 按名查工具规格（作用域过滤用）。
func toolSpecByName(name string) (toolSpec, bool) {
	for _, spec := range toolSpecs {
		if spec.Name == name {
			return spec, true
		}
	}
	return toolSpec{}, false
}

// scopeFilterMiddleware tools/list 按调用者作用域过滤可见工具。
func (s *Service) scopeFilterMiddleware(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		res, err := next(ctx, method, req)
		if err != nil || method != methodListTools {
			return res, err
		}
		list, ok := res.(*sdk.ListToolsResult)
		if !ok {
			return res, nil
		}
		level := CallerFromContext(ctx).scopeLevel()
		visible := make([]*sdk.Tool, 0, len(list.Tools))
		for _, t := range list.Tools {
			spec, known := toolSpecByName(t.Name)
			if !known || spec.MinScope <= level {
				visible = append(visible, t)
			}
		}
		list.Tools = visible
		return list, nil
	}
}

// Handler 返回 MCP 端点的 net/http 处理器（Streamable HTTP 传输）。
//
// 传输模式（对齐 MCP 规范）：
//   - 无会话（Stateless）：不写 Mcp-Session-Id；GET/DELETE 按规范回 405
//     （无会话模式不提供 SSE 流与会话终结）；
//   - JSON 响应（JSONResponse）：响应体 application/json（非 SSE 流）。
//
// 两者与本站历史行为线形一致（POST 单端点 + JSON 响应体）；协议版本协商、
// JSON-RPC 帧、工具注册与错误语义全部由官方 SDK 承担。
//
// 惰性构建（sync.Once）：请求体上限依赖 SetMaxFileSize 注入的配置，
// 首次调用时构造可保证拿到最终值。
func (s *Service) Handler() http.Handler {
	s.handlerOnce.Do(func() {
		srv := s.newMCPServer()
		s.handler = sdk.NewStreamableHTTPHandler(
			func(*http.Request) *sdk.Server { return srv },
			&sdk.StreamableHTTPOptions{
				Stateless:           true,
				JSONResponse:        true,
				MaxRequestBodyBytes: s.mcpBodyLimit(),
			},
		)
	})
	return s.handler
}

// mcpBodyLimit MCP 请求体上限：单文件上限经 base64 膨胀 4/3 后留 1MiB 余量，
// 且不低于 10MiB 地板（自研实现时代 upload.max_file_size=10MB 的口径）。
// SDK 默认仅 4MiB，会打断 share_file 的 6MB 单文件工具，故必须显式抬。
func (s *Service) mcpBodyLimit() int64 {
	const floor = 10 << 20
	limit := s.mcpMaxFileSize()*4/3 + (1 << 20)
	if limit < floor {
		return floor
	}
	return limit
}

// mcpSlogLogger 把官方 SDK 的 slog 日志桥到仓内 zap。
//
// SDK 只在 Warn/Error 级别落日志：slog 侧放行 Info 以上，落到 zap 侧统一按
// warn 收口（Error 位点极少，warn 收口可接受）；不引入自研 slog.Handler
// （仓内不留自研实现）。
func mcpSlogLogger() *slog.Logger {
	std, err := zap.NewStdLogAt(logger.With(), zapcore.WarnLevel)
	if err != nil {
		// 桥接失败（level 非法等）：退化为标准库默认 logger，不阻断 MCP 启动。
		return slog.Default()
	}
	return slog.New(slog.NewTextHandler(std.Writer(), &slog.HandlerOptions{Level: slog.LevelInfo}))
}
