package presign

// redis_unavailable_test.go —— 会话存储不可用时的**错误契约**细粒度判据
// （L6 故障注入 redis_degrade_test.go 的补充：不削弱既有断言，只加判据）。
//
// redis_degrade_test.go 已钉死「错误必须带业务码 10006」；本文件补三条运维
// 侧同样要紧、但那条用例没覆盖的性质：
//
//  ① **类型复用**：走的是 core 既有 typed error 词汇表（pkg/errors 的
//     ServiceUnavailableError），不是本域自造的平行类型 ⇒ 别的域（匿名面/
//     分片面）遇到同类故障时可直接复用同一类型与同一文案口径。
//  ② **对外脱敏**：响应体里的 message 不得含 Redis 原始文本
//     （dial tcp / connect: connection refused / 内网地址）——它会经 handler
//     的 NewTypedError 直接进 wire。
//  ③ **cause 不丢**：原始故障仍在错误链里（Unwrap）⇒ 服务端日志与
//     errors.Is/As 判据仍可追到根因，不因脱敏而丢排障信息。
//
// 另外钉住 wire 状态码：10006 经 pkg/resp 的映射必须是 **HTTP 503**
// （不是 500）——这是本缺陷修复的对外效果。

import (
	"context"
	"errors"
	"testing"

	"github.com/pigeonbox/contracts/errcode"
	pkgerrors "github.com/pigeonbox/core/pkg/errors"
	"github.com/pigeonbox/core/pkg/resp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPresign_RedisOutage_InitErrorContract 会话存储不可用 ⇒ Init 错误契约
// （类型复用 / 脱敏 / cause 保留 / 503 映射）。
func TestPresign_RedisOutage_InitErrorContract(t *testing.T) {
	svc, mr := newPresignFaultService(t)
	svc.SetShareService(&mockShareService{returnCode: "SHARE001"})
	ctx := context.Background()

	mr.Close()

	_, err := svc.Init(ctx, InitMeta{FileName: "a.txt", FileSize: 100, ContentType: "text/plain"})
	require.Error(t, err)

	// ① 类型复用：必须是 core 既有 typed error 词汇表里的那一个。
	var typed *pkgerrors.ServiceUnavailableError
	require.ErrorAs(t, err, &typed,
		"应复用 pkg/errors.ServiceUnavailableError（core 既有 typed error 词汇表），实际: %T", err)

	var coder interface{ ErrCode() int }
	require.ErrorAs(t, err, &coder)
	assert.Equal(t, errcode.CodeUnavailable, coder.ErrCode())

	// 对外效果：10006 ⇒ HTTP 503（不是 500/30008）。
	assert.Equal(t, 503, resp.HTTPStatusForCode(coder.ErrCode()),
		"「服务不可用」必须映射 503，前端才能与「参数非法」区分开")

	// ② 对外脱敏：message 不得含 Redis 原始文本/内网地址。
	msg := err.Error()
	for _, leak := range []string{"dial tcp", "connect: connection refused", "127.0.0.1", "miniredis"} {
		assert.NotContains(t, msg, leak,
			"对外文案不得含内部故障细节（会经 NewTypedError 直接进响应体）")
	}
	assert.Contains(t, msg, "会话存储", "对外文案应说明是会话存储不可用（可操作）")
	assert.Contains(t, msg, "稍后重试", "对外文案应给出下一步动作（稍后重试/退回普通上传通道）")

	// ③ cause 不丢：原始故障仍在错误链里（排障信息不因脱敏而丢）。
	cause := errors.Unwrap(err)
	require.Error(t, cause, "原始故障须保留在错误链里（Unwrap）")
	assert.NotEqual(t, msg, cause.Error(), "cause 应是底层原始故障，而非脱敏文案")
}
