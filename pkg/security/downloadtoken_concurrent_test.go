// downloadtoken_concurrent_test.go —— 下载令牌密钥的**并发读写**回归用例。
//
// ## 为什么立这条
//
// L8 安全矩阵施工时用探针实测到：`SetDownloadTokenSecret`（写）与请求处理
// 路径的读并发 ⇒ `go test -race` 报 **DATA RACE**（裸切片的头撕裂）。
// 现实里密钥多在启动期注入一次 ⇒ 属**潜伏**竞态；但密钥热更（管理台改密）
// 与请求处理并发时就会被触发 ⇒ 令牌签名结果不可预期。
//
// 本用例把「并发写 + 并发签发/校验」钉成常驻回归：**只有**在 `-race` 下跑
// 才有判据（无 -race 时竞态可能不显现 ⇒ CI 必须带 -race，本仓 CI 已是）。
//
// 负对照（实跑）：把 downloadTokenSecretPtr 改回裸切片 `var downloadTokenSecret []byte`
// ⇒ 本用例在 `go test -race` 下报 DATA RACE ⇒ 红。
package security

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDownloadTokenSecretConcurrentSetAndUse 密钥热更与令牌签发/校验并发。
func TestDownloadTokenSecretConcurrentSetAndUse(t *testing.T) {
	defer SetDownloadTokenSecret("l8-concurrent-secret-32chars-long!!")

	stop := make(chan struct{})
	var writers sync.WaitGroup
	var readers sync.WaitGroup

	// 写侧：模拟密钥热更（每次换一份等长密钥）。
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			SetDownloadTokenSecret(fmt.Sprintf("l8-concurrent-secret-%08d", i))
		}
	}()

	// 读侧：并发签发 + 校验 + 指纹。
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 300; j++ {
				tok := GenerateDownloadToken("AbCd1234")
				_ = VerifyDownloadToken("AbCd1234", tok)
				_ = VerifyDownloadTokenFingerprint("AbCd1234", tok)
			}
		}()
	}

	readers.Wait()
	close(stop)
	writers.Wait()

	// 并发跑完后密钥仍是可用的（写入侧最后一次生效）。
	tok := GenerateDownloadToken("AbCd1234")
	assert.NotEmpty(t, tok, "并发写入后密钥必须仍可用")
	assert.True(t, VerifyDownloadToken("AbCd1234", tok))
}
