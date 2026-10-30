package notify

// 通知渠道热重建的**并发安全**回归（2026-10-31）。
//
// # 缺陷形态（修复前）
//
// `Service.webhookURL`（string）与 `Service.mailer`（*SMTPMailer）是**裸字段**，
// 无任何同步：
//
//	func (s *Service) SetWebhookURL(url string) { s.webhookURL = strings.TrimSpace(url) }
//	func (s *Service) SetMailer(m *SMTPMailer)  { s.mailer = m }
//
// 而这两个 setter 由 bootstrap 的 **OnNotifyChanged 钩子在运行期**调用
// （管理员在管理台保存通知配置 ⇒ 整站热重建），读点则分布在**事件推送路径**
// （CreateForUserSimple / dispatchWebhook / sendWebhook，请求并发）⇒ **data race**。
//
// ⚠️ 且 `webhookURL` 是 **string = 16 字节**（数据指针 + 长度）⇒ 非原子写 ⇒
// 并发读可能拿到**新指针配旧长度**（撕裂）⇒ 越界读 / 崩溃。
//
// # 本文件钉住什么
//
//  1. 并发热重建 + 并发读 ⇒ 不得 data race；
//  2. 读到的 webhook 地址必须是**完整的一条 URL**（不得撕裂成半个字符串）——
//     这是「撕裂 ⇒ 崩溃」的实际后果；
//  3. **负对照**：退回裸字段写法 ⇒ 用例必须红（-race 报红 + 撕裂断言）。
//
// ⚠️ 撕裂断言在单次运行下可能碰巧不触发（需真的撞上撕裂窗口），
// 但 §1 的 -race 守门是确定性的：退回裸写法必然报 DATA RACE。

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNotify_ConcurrentHotRebuildAndRead 并发热重建 + 并发读 ⇒ 不得 race。
func TestNotify_ConcurrentHotRebuildAndRead(t *testing.T) {
	svc := NewService(nil)

	const writers = 4
	const readers = 8
	const rounds = 300

	var wg sync.WaitGroup
	start := make(chan struct{})

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				svc.SetWebhookURL(fmt.Sprintf("http://hook-%d-%d.example.com/x", w, i))
				svc.SetMailer(nil)
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				_ = svc.webhookTarget()
			}
		}()
	}

	close(start)
	wg.Wait()

	require.NotEmpty(t, svc.webhookTarget())
}

// TestNotify_WebhookURLNeverTorn 读到的地址必须是**完整的一条 URL**。
//
// 判据：地址要么为空，要么以 "http://" 开头且以 ".example.com/x" 结尾。
// 撕裂（16 字节 string 的指针与长度不匹配）会产出半个/错乱的 URL。
func TestNotify_WebhookURLNeverTorn(t *testing.T) {
	svc := NewService(nil)

	const writers = 4
	const readers = 8
	const rounds = 500

	var wg sync.WaitGroup
	start := make(chan struct{})
	broken := make(chan string, 32)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				svc.SetWebhookURL(fmt.Sprintf("http://hook-%d-%d.example.com/x", w, i))
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				got := svc.webhookTarget()
				if got == "" {
					continue
				}
				if !isWellFormedHookURL(got) {
					select {
					case broken <- got:
					default:
					}
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()
	close(broken)

	for got := range broken {
		t.Errorf("读到的 webhook 地址必须是完整的一条 URL，却得到 %q（撕裂读 ⇒ 可能越界/崩溃）", got)
	}
}

// isWellFormedHookURL 判据：形如 http://hook-<w>-<i>.example.com/x。
func isWellFormedHookURL(s string) bool {
	if len(s) < len("http://hook--.example.com/x") {
		return false
	}
	if s[:len("http://")] != "http://" {
		return false
	}
	suffix := ".example.com/x"
	if len(s) < len(suffix) || s[len(s)-len(suffix):] != suffix {
		return false
	}
	mid := s[len("http://") : len(s)-len(suffix)]
	// mid 形如 "hook-<w>-<i>"
	if len(mid) < len("hook--") || mid[:len("hook-")] != "hook-" {
		return false
	}
	return true
}

// TestNotify_SetWebhookURLTrimsAndStores 基础语义：空串 = 禁用，非空原样存。
func TestNotify_SetWebhookURLTrimsAndStores(t *testing.T) {
	svc := NewService(nil)

	svc.SetWebhookURL("   ")
	assert.Equal(t, "", svc.webhookTarget(), "空白串应归一为空（禁用）")

	svc.SetWebhookURL("  http://x.example.com/y  ")
	assert.Equal(t, "http://x.example.com/y", svc.webhookTarget(), "应去掉首尾空白")
}
