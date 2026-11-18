package conf

// 目录策略取值词表与「仅站内」判定的断言（core#233）。

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDirectoryPolicyConfig_IsInternalOnly(t *testing.T) {
	// 精确值
	assert.True(t, DirectoryPolicyConfig{ShareMode: ShareModeInternalOnly}.IsInternalOnly())
	// 大小写与首尾空白不敏感（与策略层取值归一口径一致）
	assert.True(t, DirectoryPolicyConfig{ShareMode: " Internal_Only "}.IsInternalOnly())
	// 其它取值不得误判
	assert.False(t, DirectoryPolicyConfig{ShareMode: ShareModeAllow}.IsInternalOnly())
	assert.False(t, DirectoryPolicyConfig{ShareMode: ShareModeDeny}.IsInternalOnly())
	assert.False(t, DirectoryPolicyConfig{}.IsInternalOnly())
	assert.False(t, DirectoryPolicyConfig{ShareMode: "internal_only_typo"}.IsInternalOnly())
}
