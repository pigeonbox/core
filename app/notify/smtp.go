package notify

// SMTP 邮件通知渠道（P2）。
//
// 行为：站内信创建成功后，若收件用户登记了邮箱且 SMTP 已配置，则异步补发
// 一封邮件（fire-and-forget，失败仅记日志，绝不影响站内信与主流程）。
// 端口语义：465 = 隐式 TLS；25/587 = 明文连接并在服务器支持时升级 STARTTLS。

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

	lookup UserEmailLookup
}

// UserEmailLookup 用户邮箱查询窄接口（SendToUser 用；装配点注入 dao 适配器，
// 本包不直依 repo——transport 等消费方传 nil 表示仅用 SendTo 直发模式）。
type UserEmailLookup interface {
	EmailByID(ctx context.Context, id uint) (string, error)
}

// NewSMTPMailer 构建邮件发送器（host 为空视为禁用；from 缺省取 username）。
// lookup 为 nil 时 SendToUser 直接跳过（TestSMTPNow 等只用 SendTo 的场景）。
func NewSMTPMailer(host string, port int, username, password, from string, lookup UserEmailLookup) *SMTPMailer {
	if from == "" {
		from = username
	}
	return &SMTPMailer{
		host:     strings.TrimSpace(host),
		port:     port,
		username: username,
		password: password,
		from:     from,
		lookup:   lookup,
	}
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
	m.mu.RLock()
	host, port, username, password, from := m.host, m.port, m.username, m.password, m.from
	m.mu.RUnlock()

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	msg := buildMessage(from, to, subject, body)

	auth := smtp.PlainAuth("", username, password, host)
	timeout := 15 * time.Second

	if port == 465 {
		// 隐式 TLS
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host})
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
		if ok, _ := client.Extension("AUTH"); ok && username != "" {
			if err := client.Auth(auth); err != nil {
				return fmt.Errorf("auth: %w", err)
			}
		}
		return sendWithClient(client, from, to, msg)
	}

	// 明文连接 + 可选 STARTTLS
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
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if ok, _ := client.Extension("AUTH"); ok && username != "" {
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}
	return sendWithClient(client, from, to, msg)
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
