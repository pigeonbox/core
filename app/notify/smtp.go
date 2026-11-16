package notify

// SMTP 邮件通知渠道（P2）。
//
// 行为：站内信创建成功后，若收件用户登记了邮箱且 SMTP 已配置，则异步补发
// 一封邮件（fire-and-forget，失败仅记日志，绝不影响站内信与主流程）。
//
// TLS 策略（core#222，notify.smtp.tls_mode）：
//   - required（默认）：465 隐式 TLS；其余端口必须 STARTTLS，服务器不支持
//     ⇒ 显式报错（此前 PlainAuth 在明文连接上拒绝 ⇒ 只留一行 warn）；
//   - opportunistic：465 隐式 TLS；其余明文连接、支持则 STARTTLS；配了认证
//     但服务器不支持 STARTTLS ⇒ 显式报错（不把凭据发到明文连接）；
//   - implicit：强制隐式 TLS（不看端口）；
//   - none：允许明文（显式危险 opt-in），认证使用无 TLS 校验的 PLAIN。

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/logger"
)

// SMTPMailer 邮件发送器（notify service 持有，可选）
type SMTPMailer struct {
	mu       sync.RWMutex
	host     string
	port     int
	username string
	password string
	from     string
	// tlsMode 已归一化的 TLS 策略（conf.SMTPTLS*）；configErr 非 nil 表示
	// 构造时模式非法（send 直接返回该错误，不静默回落到某个策略）。
	tlsMode   string
	configErr error

	// insecureTLS 仅测试用：自签证书的进程内 SMTP 服务需要跳过校验。
	insecureTLS bool

	lookup UserEmailLookup
}

// UserEmailLookup 用户邮箱查询窄接口（SendToUser 用；装配点注入 dao 适配器，
// 本包不直依 repo——transport 等消费方传 nil 表示仅用 SendTo 直发模式）。
type UserEmailLookup interface {
	EmailByID(ctx context.Context, id uint) (string, error)
}

// NewSMTPMailer 构建邮件发送器（host 为空视为禁用；from 缺省取 username）。
// lookup 为 nil 时 SendToUser 直接跳过（TestSMTPNow 等只用 SendTo 的场景）。
//
// tlsMode 取 notify.smtp.tls_mode（conf.SMTPTLS*）；空 = required。
// 非法值不会被静默改写：构造仍成功（保持装配面简单），但 send 返回显式
// 配置错误——管理台测试端点/发信日志都能看到「配置非法」而不是莫名鉴权失败。
func NewSMTPMailer(host string, port int, username, password, from, tlsMode string, lookup UserEmailLookup) *SMTPMailer {
	if from == "" {
		from = username
	}
	mode, modeErr := conf.NormalizeSMTPTLSMode(tlsMode)
	if modeErr != nil {
		mode = conf.SMTPTLSRequired
	}
	return &SMTPMailer{
		host:      strings.TrimSpace(host),
		port:      port,
		username:  username,
		password:  password,
		from:      from,
		tlsMode:   mode,
		configErr: modeErr,
		lookup:    lookup,
	}
}

// smtpTLSPlan 一次发送的传输策略（纯函数，便于策略矩阵测试）。
type smtpTLSPlan struct {
	// implicit 连接建立即 TLS（不发送 STARTTLS 命令）。
	implicit bool
	// requireSTARTTLS 明文连接后必须 STARTTLS，服务器不支持 ⇒ 显式失败。
	requireSTARTTLS bool
	// allowPlaintext 允许在明文连接上认证（仅 tls_mode=none）。
	allowPlaintext bool
}

// smtpTLSPlanFor 由策略 + 端口推导传输方案。
func smtpTLSPlanFor(mode string, port int) (smtpTLSPlan, error) {
	switch mode {
	case conf.SMTPTLSRequired:
		// 465 是约定俗成的 SMTPS；其余端口必须显式 STARTTLS。
		if port == 465 {
			return smtpTLSPlan{implicit: true}, nil
		}
		return smtpTLSPlan{requireSTARTTLS: true}, nil
	case conf.SMTPTLSOpportunistic:
		if port == 465 {
			return smtpTLSPlan{implicit: true}, nil
		}
		return smtpTLSPlan{}, nil // 支持则升级，不支持则明文（认证前另有拒绝）
	case conf.SMTPTLSImplicit:
		return smtpTLSPlan{implicit: true}, nil
	case conf.SMTPTLSNone:
		return smtpTLSPlan{allowPlaintext: true}, nil
	default:
		return smtpTLSPlan{}, fmt.Errorf("未知 SMTP TLS 策略: %q", mode)
	}
}

// tlsConfig 客户端 TLS 配置（测试可置 insecureTLS 以信任自签证书）。
func (m *SMTPMailer) tlsConfig(host string) *tls.Config {
	return &tls.Config{ServerName: host, InsecureSkipVerify: m.insecureTLS} //nolint:gosec // insecureTLS 仅测试注入
}

// Enabled 是否已配置
func (m *SMTPMailer) Enabled() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.host != "" && m.port > 0
}

// SendToUser 给指定用户发邮件（用户未登记邮箱时静默跳过）。
// 调用方以 goroutine fire-and-forget 使用。
func (m *SMTPMailer) SendToUser(userID uint, subject, body string) {
	if !m.Enabled() {
		return
	}
	if m.lookup == nil {
		return
	}
	email, err := m.lookup.EmailByID(context.Background(), userID)
	if err != nil || strings.TrimSpace(email) == "" {
		return
	}
	if err := m.send(email, subject, body); err != nil {
		logger.Warn("smtp mail send failed", zap.String("to", email), zap.Error(err))
	}
}

// SendTo 导出发信（管理端 SMTP 测试端点用；mail 未启用时报错）
func (m *SMTPMailer) SendTo(to, subject, body string) error {
	if !m.Enabled() {
		return errors.New("SMTP 未启用（host 为空）")
	}
	return m.send(to, subject, body)
}

func (m *SMTPMailer) send(to, subject, body string) error {
	if m.configErr != nil {
		return fmt.Errorf("SMTP TLS 配置非法: %w", m.configErr)
	}
	m.mu.RLock()
	host, port, username, password, from := m.host, m.port, m.username, m.password, m.from
	m.mu.RUnlock()

	plan, err := smtpTLSPlanFor(m.tlsMode, port)
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	msg := buildMessage(from, to, subject, body)
	timeout := 15 * time.Second

	if plan.implicit {
		conn, err := tls.Dial("tcp", addr, m.tlsConfig(host))
		if err != nil {
			return fmt.Errorf("tls dial: %w", err)
		}
		defer func() { _ = conn.Close() }()
		if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
			return fmt.Errorf("set deadline: %w", err)
		}
		client, err := smtp.NewClient(conn, host)
		if err != nil {
			return fmt.Errorf("smtp client: %w", err)
		}
		defer func() { _ = client.Close() }()
		a, aerr := smtpAuthFor(username, password, host, true /* tlsActive */, plan.allowPlaintext)
		if aerr != nil {
			return aerr
		}
		if a != nil {
			if ok, _ := client.Extension("AUTH"); ok {
				if err := client.Auth(a); err != nil {
					return fmt.Errorf("auth: %w", err)
				}
			}
		}
		return sendWithClient(client, from, to, msg)
	}

	// 明文连接（required/opportunistic 可能升级 STARTTLS；none 不升级）
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set deadline: %w", err)
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer func() { _ = client.Close() }()

	tlsActive := false
	if !plan.allowPlaintext {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(m.tlsConfig(host)); err != nil {
				return fmt.Errorf("starttls: %w", err)
			}
			tlsActive = true
		} else if plan.requireSTARTTLS {
			return fmt.Errorf("服务器不支持 STARTTLS（tls_mode=%s）；"+
				"465 端口请用 implicit/隐式 TLS，内网明文中继须显式设置 tls_mode=none", m.tlsMode)
		} else if username != "" {
			// opportunistic + 认证：绝不在明文连接上发送凭据（旧实现在这里由
			// smtp.PlainAuth 拒绝，但错误文案不可操作）。
			return fmt.Errorf("服务器不支持 STARTTLS，拒绝在明文连接上发送 SMTP 凭据"+
				"（tls_mode=%s）；确需明文认证请显式设置 tls_mode=none", m.tlsMode)
		}
	}

	a, aerr := smtpAuthFor(username, password, host, tlsActive, plan.allowPlaintext)
	if aerr != nil {
		return aerr
	}
	if a != nil {
		if ok, _ := client.Extension("AUTH"); ok {
			if err := client.Auth(a); err != nil {
				return fmt.Errorf("auth: %w", err)
			}
		}
	}
	return sendWithClient(client, from, to, msg)
}

// smtpAuthFor 选择认证实现：
//   - TLS 已建立：标准 smtp.PlainAuth（会校验 TLS/localhost）；
//   - 明文 + allowPlaintext（仅 tls_mode=none）：无 TLS 校验的 PLAIN；
//   - 明文 + 不允许：返回显式错误（绝不明文发凭据）。
func smtpAuthFor(username, password, host string, tlsActive, allowPlaintext bool) (smtp.Auth, error) {
	if username == "" {
		return nil, nil
	}
	if tlsActive {
		return smtp.PlainAuth("", username, password, host), nil
	}
	if !allowPlaintext {
		return nil, fmt.Errorf("SMTP 明文连接不允许认证（tls_mode 需 required/implicit，" +
			"或显式 none 允许明文凭据）")
	}
	// 显式 opt-in 的明文认证（仅可信内网 relay）。
	return &plainAuthNoTLS{username: username, password: password}, nil
}

// plainAuthNoTLS PLAIN 认证的「无 TLS 校验」实现（smtp.PlainAuth 在非 TLS
// 非 localhost 时硬拒绝）。只在 tls_mode=none（显式危险开关）下使用。
type plainAuthNoTLS struct {
	username string
	password string
}

func (a *plainAuthNoTLS) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if a.username == "" {
		return "", nil, errors.New("plain auth: username 为空")
	}
	return "PLAIN", []byte("\x00" + a.username + "\x00" + a.password), nil
}

func (a *plainAuthNoTLS) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("plain auth: 服务器返回了意外的挑战")
	}
	return nil, nil
}

func sendWithClient(client *smtp.Client, from, to, msg string) error {
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("rcpt to: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close body: %w", err)
	}
	return client.Quit()
}

// buildMessage 组装 MIME 邮件（UTF-8；主题/正文按 RFC 2047/2045 base64 编码以兼容中文）
func buildMessage(from, to, subject, body string) string {
	encodedSubject := "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(subject)) + "?="
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + encodedSubject + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	// 正文按 76 列折行（RFC 2045）
	enc := base64.StdEncoding.EncodeToString([]byte(body))
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return b.String()
}
