package registry

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEffective 归一语义：多主备取首个 → 去空白 → 留空回填官方公共。
//
// 搬迁前 app/p2px 与 app/federation 各存一份同值副本，此处固化为单源断言。
func TestEffective(t *testing.T) {
	// 留空 = 官方公共注册中心
	assert.Equal(t, DefaultPublic, Effective(""))
	assert.Equal(t, DefaultPublic, Effective("   "))
	// 单值去首尾空白
	assert.Equal(t, "http://example.com/", Effective(" http://example.com/ "))
	// 多主备取首个（联邦注册中心既有形态：逗号分隔主备）
	assert.Equal(t, "http://a:12346", Effective("http://a:12346,http://b:12346"))
	assert.Equal(t, "http://a:12346", Effective(" http://a:12346 , http://b:12346 "))
	// 首个为空则顺延到下一项
	assert.Equal(t, "http://b:12346", Effective(",http://b:12346"))
}

// TestPrimary 主备取首个（全空返回空串，交 Effective 回填缺省）。
func TestPrimary(t *testing.T) {
	assert.Equal(t, "http://a:1", Primary("http://a:1"))
	assert.Equal(t, "http://a:1", Primary(" http://a:1 , http://b:2 "))
	assert.Equal(t, "", Primary(" , , "))
	assert.Equal(t, "", Primary(""))
}
