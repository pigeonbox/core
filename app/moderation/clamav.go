package moderation

// ClamAV 文件病毒扫描器（外接 clamd，INSTREAM 协议）。
//
// 协议：TCP 连 clamd（默认 3310）→ 发送 "zINSTREAM\0" →
// 循环发送 [4 字节大端长度 + 数据块] → 零长度块结束 → 读响应：
//
//	stream: OK          干净
//	stream: <签名> FOUND  命中
//
// 失败策略（fail-open）：clamd 不可达/超时/响应异常时放行并记 Warn 日志——
// 可用性优先；部署方可用 Ping + 告警保障扫描器存活。
// 超过 MaxScanBytes 的文件跳过扫描（防大文件拖垮 clamd）。

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"time"
)

// ClamAVConfig clamd 连接配置（conf.ModerationConfig.ClamAV）
type ClamAVConfig struct {
	Enabled        bool   `mapstructure:"enabled"`         // 总开关（env: PB_MODERATION_CLAMAV_ENABLED）
	Addr           string `mapstructure:"addr"`            // host:port（默认 localhost:3310）
	TimeoutSeconds int    `mapstructure:"timeout_seconds"` // 单文件扫描超时，默认 60
	MaxScanBytes   int64  `mapstructure:"max_scan_bytes"`  // 超过跳过扫描（0=不限，建议 512MB）
}

// Normalized 补齐默认值
func (c ClamAVConfig) Normalized() ClamAVConfig {
	if c.Addr == "" {
		c.Addr = "localhost:3310"
	}
	if c.TimeoutSeconds <= 0 {
		c.TimeoutSeconds = 60
	}
	if c.MaxScanBytes == 0 {
		c.MaxScanBytes = 512 << 20 // 512MB
	}
	return c
}

// maxClamdReplyBytes clamd 响应读取上限（core#120）。
//
// PING 回复是 "PONG\0" 量级、INSTREAM 回复是单行 "stream: …"，4KB 富余。
// clamd 地址可配（moderation.clamav.addr）⇒ 可能指向不可信/被劫持的服务，
// 无界读（io.ReadAll(conn)）是内存放大面：deadline 之前到达的字节全部进内存。
const maxClamdReplyBytes = 1 << 12

// ContentOpener 扫描器读取文件内容的抽象（bootstrap 注入 storage service）
type ContentOpener interface {
	GetFileReader(ctx context.Context, filePath string) (io.ReadCloser, int64, error)
}

// ClamAVModerator 文件侧 Moderator：对 meta.StoragePath 指向的内容做 clamd INSTREAM 扫描。
type ClamAVModerator struct {
	cfg    ClamAVConfig
	opener ContentOpener
}

// NewClamAVModerator 构建 clamd 扫描审核器（仅文件侧；文本判定请叠加词表审核器）。
func NewClamAVModerator(addr string, timeoutSeconds int, maxScanBytes int64, opener ContentOpener) *ClamAVModerator {
	cfg := ClamAVConfig{Addr: addr, TimeoutSeconds: timeoutSeconds, MaxScanBytes: maxScanBytes}
	return &ClamAVModerator{cfg: cfg.Normalized(), opener: opener}
}

// InspectText 文件侧审核器不判文本（组合审核器把文本路由给词表）。
func (m *ClamAVModerator) InspectText(context.Context, string) Verdict { return VerdictAllow }

// Ping clamd 存活检查（部署验证/健康巡检用）
func (m *ClamAVModerator) Ping() error {
	conn, err := net.DialTimeout("tcp", m.cfg.Addr, time.Duration(m.cfg.TimeoutSeconds)*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("zPING\x00")); err != nil {
		return err
	}
	resp, err := io.ReadAll(io.LimitReader(conn, maxClamdReplyBytes))
	if err != nil {
		return err
	}
	if !strings.Contains(string(resp), "PONG") {
		return fmt.Errorf("unexpected clamd response: %q", string(resp))
	}
	return nil
}

// ScanReader 扫描任意 reader（INSTREAM）。返回 (签名, 错误)；clean 时签名为空。
func (m *ClamAVModerator) ScanReader(ctx context.Context, r io.Reader) (string, error) {
	dialer := net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", m.cfg.Addr)
	if err != nil {
		return "", fmt.Errorf("clamd 连接失败: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(time.Duration(m.cfg.TimeoutSeconds) * time.Second))

	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return "", fmt.Errorf("clamd 握手失败: %w", err)
	}

	buf := make([]byte, 32*1024)
	var sizeBuf [4]byte
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			binary.BigEndian.PutUint32(sizeBuf[:], uint32(n))
			if _, werr := conn.Write(sizeBuf[:]); werr != nil {
				return "", fmt.Errorf("clamd 发送失败: %w", werr)
			}
			if _, werr := conn.Write(buf[:n]); werr != nil {
				return "", fmt.Errorf("clamd 发送失败: %w", werr)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", fmt.Errorf("读取待扫描内容失败: %w", rerr)
		}
	}
	// 零长度块 = 流结束（sizeBuf 尚存上一块长度，必须显式清零）
	binary.BigEndian.PutUint32(sizeBuf[:], 0)
	if _, err := conn.Write(sizeBuf[:]); err != nil {
		return "", fmt.Errorf("clamd 收尾失败: %w", err)
	}

	line, err := bufio.NewReader(io.LimitReader(conn, maxClamdReplyBytes)).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("clamd 响应读取失败: %w", err)
	}
	resp := strings.TrimSpace(line)
	switch {
	case strings.HasSuffix(resp, "OK"):
		return "", nil
	case strings.HasSuffix(resp, "FOUND"):
		sig := strings.TrimSuffix(resp, " FOUND")
		sig = strings.TrimSpace(strings.TrimPrefix(sig, "stream: "))
		return sig, nil
	default:
		return "", fmt.Errorf("clamd 异常响应: %q", resp)
	}
}

// InspectFile 文件侧判定：扫描 meta.StoragePath 内容。
// 无路径/无 opener/超过上限 → 放行（记日志）；扫描器故障 → 放行（fail-open）；
// 命中签名 → 按 BlockAction 语义由调用方决定的 Verdict（此处恒 VerdictReject；
// pending 策略由 CombinedModerator/配置层转换——与词表审核器同一约定）。
func (m *ClamAVModerator) InspectFile(ctx context.Context, meta UploadMeta) Verdict {
	if meta.StoragePath == "" || m.opener == nil {
		return VerdictAllow
	}
	if m.cfg.MaxScanBytes > 0 && meta.Size > m.cfg.MaxScanBytes {
		log.Printf("[moderation] clamd skip (size %d > max %d): %s", meta.Size, m.cfg.MaxScanBytes, meta.FileName)
		return VerdictAllow
	}

	rc, _, err := m.opener.GetFileReader(ctx, meta.StoragePath)
	if err != nil {
		log.Printf("[moderation] clamd open failed (fail-open): %v", err)
		return VerdictAllow
	}
	defer func() { _ = rc.Close() }()

	sig, err := m.ScanReader(ctx, rc)
	if err != nil {
		log.Printf("[moderation] clamd scan failed (fail-open): %s: %v", meta.FileName, err)
		return VerdictAllow
	}
	if sig != "" {
		log.Printf("[moderation] clamd HIT: %s: %s", meta.FileName, sig)
		return VerdictReject
	}
	return VerdictAllow
}

// CombinedModerator 文本与文件各走独立审核器（词表 + ClamAV 组合）。
type CombinedModerator struct {
	text Moderator
	file Moderator
}

// NewCombinedModerator 组合审核器：文本判定走 text，文件判定走 file。
func NewCombinedModerator(text, file Moderator) *CombinedModerator {
	return &CombinedModerator{text: text, file: file}
}

func (m *CombinedModerator) InspectText(ctx context.Context, text string) Verdict {
	return m.text.InspectText(ctx, text)
}

func (m *CombinedModerator) InspectFile(ctx context.Context, meta UploadMeta) Verdict {
	return m.file.InspectFile(ctx, meta)
}
