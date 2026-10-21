// setup_gate.go —— 首启向导（/setup）的暴露面加固（2026-10-22）。
//
// # 问题
//
// /setup 是**无鉴权**的建号端点，唯一的护栏是「库里还没有 admin」。
// ⇒ 一个尚未初始化的实例，**任何能访问到它的人都可以抢先完成首启**并拿到
// 管理员（setup-race）。此前四个打包面把「没配密码 → 走 /setup」设为默认
// （hub/ugreen/terramaster 的 compose entrypoint、qnap 的 prepare_env），
// 这条默认把暴露面从「弱口令 admin/admin123」换成了「先到先得的管理员」——
// 在**内网**是体验升级，在**公网**是新的暴露面。
//
// # 三道加固（都不改变「不配密码也能用」的默认体验）
//
//  1. **可选令牌**：设 PB_SETUP_TOKEN 后，建号端点需带匹配的 X-Setup-Token 头
//     （或 ?token= 查询参数 —— 便于把「服务启动日志里的那条链接」直接粘进
//     浏览器完成首启）。未设 = 不要求（保持默认体验不变）。
//  2. **按 IP 失败锁定**：复用 middleware.Lockout（Redis 共享计数，多副本
//     生效；无 Redis 退进程内）。**只把「令牌不匹配」计为失败**——校验
//     参数不合法（弱口令等）是用户自己的输入问题，不该把他锁在门外。
//  3. **启动告警**：未初始化且未设令牌时，响亮提示暴露面与两种处置办法。
//
// # 为什么不用「必须令牌」
//
// 强制令牌会把「打开浏览器就走完首启」这条最顺的路变成「先翻日志抄令牌」，
// 与「多部署方式下的第一天体验」这个目标相反。故做成**可选加固**：
// 内网自托管（NAS）默认零摩擦；公网部署按需一行 env 打开。
package bootstrap

import (
	"context"
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/logger"
	"go.uber.org/zap"
)

// setupTokenEnv 首启令牌的环境变量名。
const setupTokenEnv = "PB_SETUP_TOKEN"

// setupTokenHeader 携带首启令牌的请求头名。
const setupTokenHeader = "X-Setup-Token"

// setupTokenCookie 携带首启令牌的 Cookie 名（前端向导用，避免改动 IDL 契约）。
const setupTokenCookie = "pb_setup_token"

// setupToken 返回配置的首启令牌（未配置时返回空串 = 不要求令牌）。
// 每次请求读 env：容器编排改 env 需重启才生效，读 env 本身开销可忽略，
// 但**不缓存**——避免「改了 env 但门还认旧值」这种难查的错配。
func setupToken() string {
	return strings.TrimSpace(os.Getenv(setupTokenEnv))
}

// setupGate 返回首启门禁中间件（nil = 不启用）。
//
// 只拦**建号**动作（POST /setup）：/setup/check 是无害读（返回是否已初始化），
// 前端首屏要靠它决定跳不跳转向导，拦它等于把首屏也锁死。
func setupGate() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		path := string(c.Request.URI().Path())
		if path != "/setup" || string(c.Request.Method()) != http.MethodPost {
			c.Next(ctx)
			return
		}
		// 已初始化：/setup 本就无效（服务层会拒「系统已初始化」），不拦。
		if svc := publicSetupSvc; svc != nil {
			if initialized, err := svc.IsSystemInitialized(ctx); err == nil && initialized {
				c.Next(ctx)
				return
			}
		}
		token := setupToken()
		if token == "" {
			// 未配置令牌 = 不要求（默认体验）；暴露面由启动告警提示。
			c.Next(ctx)
			return
		}
		// 失败锁定：只统计「令牌不匹配」。
		lock := middleware.GetDefaultLockout()
		lockKey := middleware.FormatLockKey("setup", middleware.ClientIPFromContext(ctx))
		if remain, locked := lock.CheckLocked(ctx, lockKey); locked {
			c.JSON(consts.StatusTooManyRequests, map[string]interface{}{
				"code":    429,
				"message": "首启令牌校验失败次数过多，请稍后再试",
				"remain":  remain,
			})
			c.Abort()
			return
		}
		if !setupTokenMatch(c, token) {
			lock.RecordFailure(ctx, lockKey)
			c.JSON(consts.StatusUnauthorized, map[string]interface{}{
				"code":    401,
				"message": "首启令牌无效：请在服务启动日志中查找首启令牌，或用带 ?token= 的链接打开本页",
			})
			c.Abort()
			return
		}
		lock.Reset(ctx, lockKey)
		c.Next(ctx)
	}
}

// setupTokenMatch 比对请求携带的令牌与配置令牌（常量时间，防时序侧信道）。
// 令牌来源（任一命中即算携带）：
//   - X-Setup-Token 请求头；
//   - ?token= 查询参数（便于把「服务启动日志里的那条链接」直接粘进浏览器）；
//   - pb_setup_token Cookie（前端向导用；避免为了传一个值去改 IDL 契约的
//     请求体 —— setup.thrift 的 InitializeReq 是强类型的，加字段要走
//     contracts 发布 + 前端抬钉版，代价远大于收益）。
func setupTokenMatch(c *app.RequestContext, want string) bool {
	got := strings.TrimSpace(string(c.Request.Header.Peek(setupTokenHeader)))
	if got == "" {
		got = strings.TrimSpace(c.Query("token"))
	}
	if got == "" {
		got = strings.TrimSpace(string(c.Cookie(setupTokenCookie)))
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// warnSetupExposure 未初始化且未配置令牌时，响亮提示首启暴露面。
//
// 这条告警不是"建议加个令牌"，而是**陈述事实**：此刻任何能访问到本实例的
// 人都可以抢先建号。公网部署必须处置（设 PB_SETUP_TOKEN 或走内网）。
func warnSetupExposure(ctx context.Context) {
	if setupToken() != "" {
		return // 已配置令牌 = 暴露面已收口
	}
	svc := publicSetupSvc
	if svc == nil {
		return
	}
	initialized, err := svc.IsSystemInitialized(ctx)
	if err != nil {
		return
	}
	if initialized {
		return
	}
	logger.Warn("首启向导未设令牌：任何能访问到本实例的人都可以抢先完成首启并获得管理员",
		zap.String("hint", "公网部署请设 PB_SETUP_TOKEN（令牌见启动日志）；内网自托管可忽略本条"))
}
