// core#222：SMTP TLS 策略矩阵 + 无 STARTTLS 服务器的显式行为。
//
// 修复前：按端口猜加密 + 固定 smtp.PlainAuth ⇒ 无 STARTTLS 的内网 relay
// 在 AUTH 阶段被 PlainAuth 拒绝（非 TLS 非 localhost），整条发信链路恒失败，
// 且调用方只看到一行 warn。现在：
//   - required（默认）：无 STARTTLS ⇒ 明确报错「服务器不支持 STARTTLS」；
//   - opportunistic：无 STARTTLS + 无认证 ⇒ 明文发送（旧行为）；配了认证 ⇒
//     明确报错「拒绝在明文连接上发送凭据」；
//   - none（显式危险 opt-in）：允许明文认证；
//   - implicit：强制隐式 TLS。
package notify

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
)

// ===== 策略矩阵（纯函数）=====

func TestSMTPTLSPlanMatrix(t *testing.T) {
	cases := []struct {
		name            string
		mode            string
		port            int
		implicit        bool
		requireSTARTTLS bool
		allowPlaintext  bool
	}{
		{"required+465 ⇒ 隐式 TLS", conf.SMTPTLSRequired, 465, true, false, false},
		{"required+587 ⇒ 强制 STARTTLS", conf.SMTPTLSRequired, 587, false, true, false},
		{"required+25 ⇒ 强制 STARTTLS", conf.SMTPTLSRequired, 25, false, true, false},
		{"opportunistic+465 ⇒ 隐式 TLS", conf.SMTPTLSOpportunistic, 465, true, false, false},
		{"opportunistic+587 ⇒ 机会式（不强制）", conf.SMTPTLSOpportunistic, 587, false, false, false},
		{"implicit+25 ⇒ 隐式 TLS（不看端口）", conf.SMTPTLSImplicit, 25, true, false, false},
		{"none+587 ⇒ 明文（显式 opt-in）", conf.SMTPTLSNone, 587, false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := smtpTLSPlanFor(c.mode, c.port)
			require.NoError(t, err)
			assert.Equal(t, c.implicit, p.implicit)
			assert.Equal(t, c.requireSTARTTLS, p.requireSTARTTLS)
			assert.Equal(t, c.allowPlaintext, p.allowPlaintext)
		})
	}
	_, err := smtpTLSPlanFor("bogus", 25)
	require.Error(t, err)
}

// ===== 进程内 SMTP 测试服务 =====

type smtpServerOpts struct {
	advertiseSTARTTLS bool
	implicitTLS       bool
}

type smtpServerState struct {
	mu           sync.Mutex
	authSeen     bool
	startTLSUsed bool
	mailFrom     string
	data         string
}

func (s *smtpServerState) setAuth()     { s.mu.Lock(); s.authSeen = true; s.mu.Unlock() }
func (s *smtpServerState) setStartTLS() { s.mu.Lock(); s.startTLSUsed = true; s.mu.Unlock() }
func (s *smtpServerState) setMail(v string) {
	s.mu.Lock()
	s.mailFrom = v
	s.mu.Unlock()
}
func (s *smtpServerState) setData(v string) { s.mu.Lock(); s.data = v; s.mu.Unlock() }
func (s *smtpServerState) snapshot() (auth, startTLS bool, from, data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authSeen, s.startTLSUsed, s.mailFrom, s.data
}

func testTLSCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "pb-smtp-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func startSMTPTestServer(t *testing.T, opts smtpServerOpts) (string, int, *smtpServerState) {
	t.Helper()
	cert := testTLSCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	st := &smtpServerState{}

	serveLn := net.Listener(ln)
	if opts.implicitTLS {
		serveLn = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
	}
	go func() {
		for {
			c, err := serveLn.Accept()
			if err != nil {
				return
			}
			go serveSMTPConn(c, cert, opts, st)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	return host, port, st
}

func serveSMTPConn(conn net.Conn, cert tls.Certificate, opts smtpServerOpts, st *smtpServerState) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	write := func(s string) { _, _ = conn.Write([]byte(s)) }
	write("220 pb-test ESMTP\r\n")

	tlsActive := opts.implicitTLS
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			write("250-pb-test\r\n")
			if opts.advertiseSTARTTLS && !tlsActive {
				write("250-STARTTLS\r\n")
			}
			write("250-AUTH PLAIN\r\n")
			write("250 OK\r\n")
		case strings.HasPrefix(upper, "STARTTLS"):
			if !opts.advertiseSTARTTLS || tlsActive {
				write("454 TLS not available\r\n")
				continue
			}
			write("220 2.0.0 Ready to start TLS\r\n")
			tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			r = bufio.NewReader(conn)
			tlsActive = true
			st.setStartTLS()
		case strings.HasPrefix(upper, "AUTH"):
			st.setAuth()
			write("235 2.7.0 Authentication successful\r\n")
		case strings.HasPrefix(upper, "MAIL FROM"):
			st.setMail(line)
			write("250 OK\r\n")
		case strings.HasPrefix(upper, "RCPT TO"):
			write("250 OK\r\n")
		case strings.HasPrefix(upper, "DATA"):
			write("354 End data with <CR><LF>.<CR><LF>\r\n")
			var data strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				data.WriteString(l)
			}
			st.setData(data.String())
			write("250 OK: queued\r\n")
		case strings.HasPrefix(upper, "QUIT"):
			write("221 Bye\r\n")
			return
		default:
			write("250 OK\r\n")
		}
	}
}

func newTestMailer(host string, port int, username, password, mode string) *SMTPMailer {
	m := NewSMTPMailer(host, port, username, password, "sender@example.com", mode, nil)
	m.insecureTLS = true // 自签证书的进程内服务
	return m
}

// ===== 行为矩阵（进程内真实 SMTP 会话）=====

func TestSMTP_TLSModeBehavior(t *testing.T) {
	t.Run("required：无 STARTTLS ⇒ 显式报错且不发 AUTH", func(t *testing.T) {
		host, port, st := startSMTPTestServer(t, smtpServerOpts{})
		err := newTestMailer(host, port, "u", "p", conf.SMTPTLSRequired).
			SendTo("to@example.com", "s", "b")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "不支持 STARTTLS")
		auth, startTLS, _, data := st.snapshot()
		assert.False(t, auth, "required+无 STARTTLS 时不得发送 AUTH")
		assert.False(t, startTLS)
		assert.Empty(t, data)
	})

	t.Run("required：支持 STARTTLS ⇒ 升级后认证并发送", func(t *testing.T) {
		host, port, st := startSMTPTestServer(t, smtpServerOpts{advertiseSTARTTLS: true})
		err := newTestMailer(host, port, "u", "p", conf.SMTPTLSRequired).
			SendTo("to@example.com", "s", "b")
		require.NoError(t, err)
		auth, startTLS, from, data := st.snapshot()
		assert.True(t, startTLS, "required 必须使用 STARTTLS")
		assert.True(t, auth, "TLS 建立后应发送认证")
		assert.Contains(t, from, "MAIL FROM")
		assert.NotEmpty(t, data)
	})

	t.Run("opportunistic：无 STARTTLS + 认证 ⇒ 显式拒绝明文凭据", func(t *testing.T) {
		host, port, st := startSMTPTestServer(t, smtpServerOpts{})
		err := newTestMailer(host, port, "u", "p", conf.SMTPTLSOpportunistic).
			SendTo("to@example.com", "s", "b")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "明文连接上发送 SMTP 凭据")
		auth, _, _, _ := st.snapshot()
		assert.False(t, auth, "绝不能在明文连接上发送 AUTH")
	})

	t.Run("opportunistic：无 STARTTLS + 无认证 ⇒ 明文发送（旧行为）", func(t *testing.T) {
		host, port, st := startSMTPTestServer(t, smtpServerOpts{})
		err := newTestMailer(host, port, "", "", conf.SMTPTLSOpportunistic).
			SendTo("to@example.com", "s", "b")
		require.NoError(t, err)
		auth, startTLS, _, data := st.snapshot()
		assert.False(t, auth)
		assert.False(t, startTLS)
		assert.NotEmpty(t, data)
	})

	t.Run("none：无 STARTTLS + 认证 ⇒ 显式 opt-in 后明文认证", func(t *testing.T) {
		host, port, st := startSMTPTestServer(t, smtpServerOpts{})
		err := newTestMailer(host, port, "u", "p", conf.SMTPTLSNone).
			SendTo("to@example.com", "s", "b")
		require.NoError(t, err)
		auth, startTLS, _, data := st.snapshot()
		assert.True(t, auth, "tls_mode=none 应允许明文 PLAIN 认证")
		assert.False(t, startTLS, "none 不得发送 STARTTLS")
		assert.NotEmpty(t, data)
	})

	t.Run("implicit：不看端口，连接即 TLS", func(t *testing.T) {
		host, port, st := startSMTPTestServer(t, smtpServerOpts{implicitTLS: true})
		err := newTestMailer(host, port, "u", "p", conf.SMTPTLSImplicit).
			SendTo("to@example.com", "s", "b")
		require.NoError(t, err)
		auth, _, _, data := st.snapshot()
		assert.True(t, auth)
		assert.NotEmpty(t, data)
	})

	t.Run("非法 tls_mode ⇒ 构造期错误，拨号前拒绝", func(t *testing.T) {
		m := newTestMailer("127.0.0.1", 1, "u", "p", "bogus")
		err := m.SendTo("to@example.com", "s", "b")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "TLS 配置非法")
	})
}

func TestNormalizeSMTPTLSMode(t *testing.T) {
	for in, want := range map[string]string{
		"":              conf.SMTPTLSRequired,
		"required":      conf.SMTPTLSRequired,
		" STARTTLS ":    conf.SMTPTLSRequired,
		"opportunistic": conf.SMTPTLSOpportunistic,
		"implicit":      conf.SMTPTLSImplicit,
		"tls":           conf.SMTPTLSImplicit,
		"none":          conf.SMTPTLSNone,
		"plaintext":     conf.SMTPTLSNone,
	} {
		got, err := conf.NormalizeSMTPTLSMode(in)
		require.NoErrorf(t, err, "输入 %q", in)
		assert.Equalf(t, want, got, "输入 %q", in)
	}
	_, err := conf.NormalizeSMTPTLSMode("ssl")
	require.Error(t, err)
}
