package updatecheck

// 更新检查（2026-11-06，5.0.0）——回答一个问题：
// **这台机器知不知道自己落后了？**
//
// # 为什么要做
//
// 4.9.1 之前，生态里没有任何「检查更新」能力：core 无实现、`pb` 无子命令、
// 管理台维护页只有「清理过期 / 优化 / 系统信息」。后果是**修好的安全缺陷装不到
// 用户机器上**——某台 fnOS 真机在 4.1.2 停了约三周，而 4.8.6 修的正是
// 「管理员口令重置丢归属 + 硬删泄漏子表」。继续加功能只会让升级落差更大。
//
// # 三条硬约束（决定了本包长什么样）
//
//  1. **离线不是故障**。NAS 常常没有出站网络或出站被墙。拿不到结果必须表达成
//     `source=unknown`，而不是让 `pb doctor` 报红——否则用户会以为自己的
//     部署坏了（与 server#88「缺 API Key 被判成服务故障」同型的错误）。
//  2. **不能对上游造成请求风暴**。GitHub 未认证 API 每小时只有 60 次；
//     管理台每打开一次就查一次的话，刷新几下就耗尽配额。⇒ 24h 落盘缓存 +
//     并发合并（多个同时调用只发一次请求）。
//  3. **不能泄露实例信息**。请求里不带站点 id、分享码、用户、部署模式；
//     User-Agent 只带公开的版本号（CVE 匹配本来就要公开版本）。
//
// # 与本包无关的事
//
// 这里**不做**升级动作（不下载、不替换二进制、不重启）。那属于各平台打包层
// （fpk / qpkg / ipk / 镜像）的职责，跨平台统一在这里做只会做错。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 结果来源（Result.Source）。
const (
	// SourceGitHub 拿到了上游的真实数据（可能是本次请求，也可能来自缓存）。
	SourceGitHub = "github"
	// SourceDisabled 管理员显式关闭了检查。
	SourceDisabled = "disabled"
	// SourceUnknown 查不到（离线、超时、上游异常、响应不可解析）。
	// ⚠️ 这不是错误状态，是「不知道」。
	SourceUnknown = "unknown"
)

// 默认值（bootstrap 装配时可覆盖）。
const (
	defaultTTL       = 24 * time.Hour
	defaultTimeout   = 5 * time.Second
	defaultUserAgent = "pigeonbox-update-check"
	githubLatestURL  = "https://api.github.com/repos/pigeonbox/pigeonbox/releases/latest"
	cacheFileName    = "update-check.json"
	cacheFileMode    = 0o600
	cacheDirMode     = 0o755
)

// Config 检查器配置（由 bootstrap 装配）。
type Config struct {
	// Enabled 是否启用检查（update.check_enabled / PB_UPDATE_CHECK_ENABLED）。
	Enabled bool
	// Endpoint 查询地址。默认 GitHub Releases API；
	// 允许自建镜像（企业内网），但**必须是 https**（localhost 例外，供测试）。
	Endpoint string
	// Current 当前构建版本（oekit/core/version.Version）。
	Current string
	// CachePath 缓存文件绝对路径（落盘失败时自动降级为纯内存）。
	CachePath string
	// TTL 缓存有效期，默认 24h。
	TTL time.Duration
	// Timeout 单次请求超时，默认 5s。
	Timeout time.Duration
	// UserAgent 出站 UA（只允许公开信息）。
	UserAgent string
}

// Result 一次检查的结论（可直接序列化进 /admin/diagnostics）。
type Result struct {
	// Current 当前版本。
	Current string `json:"current"`
	// Latest 上游最新版本（未知时为空）。
	Latest string `json:"latest,omitempty"`
	// Available 是否有更新（Latest 严格新于 Current）。
	Available bool `json:"available"`
	// Source github | disabled | unknown。
	Source string `json:"source"`
	// CheckedAt 这条数据的取得时间（缓存命中时是缓存时间，不是当前时间）。
	CheckedAt time.Time `json:"checked_at,omitempty"`
	// Stale true = 本次请求失败，返回的是过期缓存（提示运维「这条可能不准」）。
	Stale bool `json:"stale,omitempty"`
	// URL 上游 release 页面（前端做「查看升级说明」入口）。
	URL string `json:"url,omitempty"`
	// Error 取不到最新版本的**可读原因**（source=unknown 或 stale 时非空）。
	Error string `json:"error,omitempty"`
}

// cacheFile 落盘缓存形态。
type cacheFile struct {
	Latest    string    `json:"latest"`
	URL       string    `json:"url,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// githubRelease GitHub Releases API 的响应（只取用到的字段）。
type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// Checker 并发安全的更新检查器。
//
// 零值不可用，一律走 New。
type Checker struct {
	cfg    Config
	client *http.Client

	// mu 保护 mem 与「同一时刻只打一次上游」。
	mu     sync.Mutex
	mem    *cacheFile
	flight *flight
}

// flight 一次进行中的请求（并发调用合并到同一次）。
type flight struct {
	done chan struct{}
	res  Result
}

// New 构造检查器。
func New(cfg Config) *Checker {
	if cfg.TTL <= 0 {
		cfg.TTL = defaultTTL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = githubLatestURL
	}
	return &Checker{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.Timeout,
			// 不跟随跳转：GitHub API 正常不跳；跟随会让「配置被改成任意 URL」的
			// 影响面变大（跳转目标不受 https 校验约束）。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Check 返回更新状态。
//
// force=false 时优先用未过期缓存；force=true 强制打一次上游
// （管理台「立即检查」按钮用）。
//
// ⚠️ 本方法**永不 panic、永不返回错误**：更新检查是「锦上添花」，
// 任何失败都必须退化成 source=unknown，绝不能把诊断端点带崩。
func (c *Checker) Check(ctx context.Context, force bool) Result {
	if !c.cfg.Enabled {
		return Result{Current: c.cfg.Current, Source: SourceDisabled}
	}

	if !force {
		if cached, ok := c.freshCache(); ok {
			return resultFromCache(c.cfg.Current, cached, false, "")
		}
	}

	// 并发合并：同一时刻只打一次上游，其余调用等这一次的结果。
	c.mu.Lock()
	if c.flight != nil {
		f := c.flight
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.res
		case <-ctx.Done():
			return Result{Current: c.cfg.Current, Source: SourceUnknown, Error: ctx.Err().Error()}
		}
	}
	f := &flight{done: make(chan struct{})}
	c.flight = f
	c.mu.Unlock()

	res := c.fetch(ctx)

	c.mu.Lock()
	f.res = res
	c.flight = nil
	c.mu.Unlock()
	close(f.done)
	return res
}

// freshCache 取未过期缓存（内存优先，其次磁盘）。
func (c *Checker) freshCache() (*cacheFile, bool) {
	c.mu.Lock()
	mem := c.mem
	c.mu.Unlock()
	if mem != nil && time.Since(mem.CheckedAt) < c.cfg.TTL {
		return mem, true
	}
	if cf, ok := c.loadCache(); ok && time.Since(cf.CheckedAt) < c.cfg.TTL {
		return cf, true
	}
	return nil, false
}

// staleCache 取任意缓存（用于请求失败时的降级展示）。
func (c *Checker) staleCache() *cacheFile {
	c.mu.Lock()
	mem := c.mem
	c.mu.Unlock()
	if mem != nil {
		return mem
	}
	if cf, ok := c.loadCache(); ok {
		return cf
	}
	return nil
}

// fetch 真正打一次上游。
func (c *Checker) fetch(ctx context.Context) Result {
	if err := validateEndpoint(c.cfg.Endpoint); err != nil {
		return Result{
			Current: c.cfg.Current,
			Source:  SourceUnknown,
			Error:   err.Error(),
		}
	}

	reqCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.cfg.Endpoint, nil)
	if err != nil {
		return c.degrade(fmt.Errorf("构造请求失败: %w", err))
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", c.cfg.UserAgent+"/"+strings.TrimPrefix(c.cfg.Current, "v"))

	resp, err := c.client.Do(req)
	if err != nil {
		return c.degrade(fmt.Errorf("请求上游失败: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return c.degrade(fmt.Errorf("上游返回 HTTP %d", resp.StatusCode))
	}

	var rel githubRelease
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&rel); err != nil {
		return c.degrade(fmt.Errorf("解析上游响应失败: %w", err))
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return c.degrade(errors.New("上游响应缺少 tag_name"))
	}

	cf := &cacheFile{Latest: rel.TagName, URL: rel.HTMLURL, CheckedAt: time.Now().UTC()}
	c.storeCache(cf)
	return resultFromCache(c.cfg.Current, cf, false, "")
}

// degrade 请求失败时的统一出口：有旧缓存就带旧值（标 stale），否则 unknown。
func (c *Checker) degrade(err error) Result {
	if cf := c.staleCache(); cf != nil {
		return resultFromCache(c.cfg.Current, cf, true, err.Error())
	}
	return Result{Current: c.cfg.Current, Source: SourceUnknown, Error: err.Error()}
}

// resultFromCache 把缓存形态转成对外结果。
func resultFromCache(current string, cf *cacheFile, stale bool, errMsg string) Result {
	return Result{
		Current:   current,
		Latest:    cf.Latest,
		Available: isNewer(cf.Latest, current),
		Source:    SourceGitHub,
		CheckedAt: cf.CheckedAt,
		Stale:     stale,
		URL:       cf.URL,
		Error:     errMsg,
	}
}

// storeCache 内存 + 落盘（落盘失败只降级，不影响结果）。
func (c *Checker) storeCache(cf *cacheFile) {
	c.mu.Lock()
	c.mem = cf
	c.mu.Unlock()

	if c.cfg.CachePath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.cfg.CachePath), cacheDirMode); err != nil {
		return
	}
	buf, err := json.Marshal(cf)
	if err != nil {
		return
	}
	// 先写临时文件再 rename：避免进程在写一半时被杀，留下半截 JSON
	// （下次启动解析失败 ⇒ 缓存静默失效，看起来像「每次都在查」）。
	tmp := c.cfg.CachePath + ".tmp"
	if err := os.WriteFile(tmp, buf, cacheFileMode); err != nil {
		return
	}
	_ = os.Rename(tmp, c.cfg.CachePath)
}

// loadCache 从磁盘读缓存。
func (c *Checker) loadCache() (*cacheFile, bool) {
	if c.cfg.CachePath == "" {
		return nil, false
	}
	buf, err := os.ReadFile(c.cfg.CachePath)
	if err != nil {
		return nil, false
	}
	var cf cacheFile
	if err := json.Unmarshal(buf, &cf); err != nil {
		return nil, false
	}
	if cf.Latest == "" || cf.CheckedAt.IsZero() {
		return nil, false
	}
	c.mu.Lock()
	c.mem = &cf
	c.mu.Unlock()
	return &cf, true
}

// validateEndpoint 只允许 https（localhost 例外，供测试与内网自建镜像）。
//
// 理由：这个地址可被配置改动。若允许任意 http，一次配置错误就能把
// 「当前版本 + 出站请求」发到明文链路上；而 localhost 在测试与
// 「同机镜像」场景下没有这个问题。
func validateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("上游地址不可解析: %w", err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && isLoopback(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("上游地址必须使用 https（localhost 除外），当前为 %q", u.Scheme)
}

// isLoopback 判断主机名是否本机回环。
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isNewer 判断 latest 是否严格新于 current（两者都形如 v4.9.1 / 4.9.1）。
//
// 解析失败一律返回 false —— 宁可漏报一次，也不要因为版本号里出现
// 非预期形态（如 v4.0.7-1-gb8c15f1）就误报「有新版本」。
func isNewer(latest, current string) bool {
	lv, lok := parseSemver(latest)
	cv, cok := parseSemver(current)
	if !lok || !cok {
		return false
	}
	for i := 0; i < 3; i++ {
		if lv[i] != cv[i] {
			return lv[i] > cv[i]
		}
	}
	return false
}

// parseSemver 取版本号前三段数字（忽略前缀 v 与 -pre / +build 后缀）。
func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	s := strings.TrimSpace(v)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return out, false
	}
	// 去掉预发布/构建元数据：-1-gb8c15f1、+meta
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 {
		return out, false
	}
	for i := 0; i < 3; i++ {
		if i >= len(parts) {
			out[i] = 0
			continue
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
