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
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/middleware"
	"go.uber.org/zap"
)

// setupTokenEnv 首启令牌的环境变量名。
const setupTokenEnv = "PB_SETUP_TOKEN"

// setupTokenAutoEnv 自动生成首启令牌的开关环境变量名。
//
// 默认**开启**（未设 = 自动生成一次性令牌）；设为 0/false/off/no 可关闭，
// 关闭后回到「不要求令牌 + 启动告警」的旧行为。
const setupTokenAutoEnv = "PB_SETUP_TOKEN_AUTO"

// setupTokenHeader 携带首启令牌的请求头名。
const setupTokenHeader = "X-Setup-Token"

// setupTokenCookie 携带首启令牌的 Cookie 名（前端向导用，避免改动 IDL 契约）。
const setupTokenCookie = "pb_setup_token"

// setupAutoToken 启动时**自动生成**的一次性首启令牌（进程内）。
//
// 为什么要有它：此前令牌只能由运维**手工**配 PB_SETUP_TOKEN —— 而
// 「没配」正是暴露面最大的那一种（任何能访问到实例的人都能抢先建号）。
// ⇒ 改成「启动时自动生成一个、打印在启动日志里」：
// 暴露面默认收口，且运维**零配置**（令牌从日志里复制即可）。
//
// 显式 PB_SETUP_TOKEN 优先级更高（运维自选口令时用它）。
var setupAutoToken atomic.Pointer[string]

// setupTokenAutoEnabled 自动生成令牌是否启用（默认开）。
func setupTokenAutoEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(setupTokenAutoEnv))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// setupToken 返回配置的首启令牌（未配置时返回空串 = 不要求令牌）。
// 每次请求读 env：容器编排改 env 需重启才生效，读 env 本身开销可忽略，
// 但**不缓存**——避免「改了 env 但门还认旧值」这种难查的错配。
func setupToken() string {
	return strings.TrimSpace(os.Getenv(setupTokenEnv))
}

// effectiveSetupToken 生效的首启令牌：显式 env 优先，否则用自动生成的一次性令牌。
//
// 空串 = 不要求令牌（默认体验零摩擦；仅当自动生成被显式关闭且未配 env 时出现）。
func effectiveSetupToken() string {
	if t := setupToken(); t != "" {
		return t
	}
	if p := setupAutoToken.Load(); p != nil {
		return *p
	}
	return ""
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
		token := effectiveSetupToken()
		if token == "" {
			// 未启用令牌 = 不要求（默认体验）；暴露面由启动告警提示。
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
			// ⚠️ **缺令牌不算失败**（2026-10-30 修正）：
			// 自动生成令牌默认开启后，「打开页面还没填令牌就点初始化」是
			// **正常流程**（前端拿到 401 后才弹出令牌输入框）⇒ 若把它计入
			// 按 IP 的失败锁定，运维照着日志填令牌的那一次点击就可能已把
			// 预算耗尽 ⇒ 明明有正确令牌却被锁在门外（429）。
			// 只有「**提交了**令牌但猜错」才算攻击尝试。
			if setupTokenProvided(c) {
				lock.RecordFailure(ctx, lockKey)
			}
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

// setupTokenProvided 请求是否**携带了**令牌（任一通道命中即算携带）。
//
// 与 setupTokenMatch 的区别：前者判「有没有带」，后者判「带的对不对」。
// 缺令牌是「还没提交凭据」，猜错令牌才是「猜错了」。
func setupTokenProvided(c *app.RequestContext) bool {
	if strings.TrimSpace(string(c.Request.Header.Peek(setupTokenHeader))) != "" {
		return true
	}
	if strings.TrimSpace(c.Query("token")) != "" {
		return true
	}
	if strings.TrimSpace(string(c.Cookie(setupTokenCookie))) != "" {
		return true
	}
	return false
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

// warnSetupExposure 未初始化且**未启用任何令牌**时，响亮提示首启暴露面。
//
// 这条告警不是"建议加个令牌"，而是**陈述事实**：此刻任何能访问到本实例的
// 人都可以抢先建号。仅当自动生成被显式关闭（PB_SETUP_TOKEN_AUTO=0）且未配
// PB_SETUP_TOKEN 时才会出现。
func warnSetupExposure(ctx context.Context) {
	if effectiveSetupToken() != "" {
		return // 令牌已生效 = 暴露面已收口
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
	logger.Warn("首启向导未启用令牌：任何能访问到本实例的人都可以抢先完成首启并获得管理员",
		zap.String("hint", "已默认自动生成一次性令牌（见启动日志）；如需关闭请设 PB_SETUP_TOKEN_AUTO=0，或自行设置 PB_SETUP_TOKEN"))
}

// ensureSetupToken 启动时准备首启令牌（自动生成一次性令牌 + 打印 + 落文件）。
//
// 触发条件（三者同时满足）：
//
//	① 未显式设置 PB_SETUP_TOKEN（运维自选口令优先）；
//	② 自动生成未被关闭（PB_SETUP_TOKEN_AUTO=0/off/false/no）；
//	③ 系统尚未初始化（已有 admin ⇒ 首启面已关闭，不需要令牌）。
//
// 令牌是**进程内**的（重启会换新）——这正是"一次性令牌"的语义：
// 它防的是「未初始化实例被他人抢先建号」，不是长期凭据。
//
// 落地两处（运维两条路都能拿到）：
//   - 启动日志（醒目横幅 + 可直接打开的链接）；
//   - 数据目录下的 .setup_token 文件（0600）——NAS 用户不必去翻日志。
func ensureSetupToken(ctx context.Context) {
	if setupToken() != "" {
		return // 显式 env 优先
	}
	if !setupTokenAutoEnabled() {
		return // 运维显式关闭自动生成
	}
	if publicSetupSvc == nil {
		return
	}
	initialized, err := publicSetupSvc.IsSystemInitialized(ctx)
	if err != nil {
		logger.Warn("首启令牌：查询初始化状态失败，跳过自动生成", zap.Error(err))
		return
	}
	if initialized {
		return // 已初始化 ⇒ 首启面已关闭
	}

	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		logger.Error("首启令牌：生成失败（本次实例不启用令牌门禁）", zap.Error(err))
		return
	}
	token := hex.EncodeToString(buf)
	setupAutoToken.Store(&token)

	// 启动日志：令牌本体 + 可直接打开的链接（两种都给出，兼容不同部署形态）。
	base := bootstrapBaseURL()
	logger.Info("首启令牌已生成（一次性，仅本次进程有效）："+token,
		zap.String("token", token),
		zap.String("setup_url", fmt.Sprintf("%s/?token=%s#/setup", base, token)),
		zap.String("hint", "打开上面的链接即可完成首启；也可在向导里粘贴令牌"))
	// 数据目录落文件（NAS 用户不必翻日志）：0600，只有属主可读。
	if p := setupTokenFilePath(); p != "" {
		if err := os.WriteFile(p, []byte(token), 0o600); err != nil {
			logger.Warn("首启令牌：写入文件失败（仍可从启动日志复制）",
				zap.String("path", p), zap.Error(err))
		} else {
			logger.Info("首启令牌已写入文件", zap.String("path", p))
		}
	}
}

// setupTokenFilePath 令牌文件路径（数据目录下的 .setup_token）；无数据目录时返回空串。
func setupTokenFilePath() string {
	if config == nil {
		return ""
	}
	dataPath := strings.TrimSpace(config.App.DataPath)
	if dataPath == "" {
		return ""
	}
	abs, err := filepath.Abs(dataPath)
	if err != nil {
		abs = dataPath
	}
	return filepath.Join(abs, ".setup_token")
}

// clearSetupToken 首启成功后清理一次性令牌（进程内 + 文件）。
//
// 由 app/setup 的 OnInitialized 钩子调用（装配层注入，app/setup 不反向依赖
// bootstrap）。清理后令牌不再有效——首启面已关闭，留着也只是垃圾。
func clearSetupToken() {
	setupAutoToken.Store(nil)
	if p := setupTokenFilePath(); p != "" {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			logger.Warn("首启令牌：清理文件失败", zap.String("path", p), zap.Error(err))
		}
	}
}
