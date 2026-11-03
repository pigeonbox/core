package memkv

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetSet_Basic 覆盖写入/读取/覆盖语义。
func TestGetSet_Basic(t *testing.T) {
	s := New()
	_, ok := s.Get("k")
	assert.False(t, ok, "空表不应命中")

	s.Set("k", "v1", time.Minute)
	v, ok := s.Get("k")
	require.True(t, ok)
	assert.Equal(t, "v1", v)

	s.Set("k", "v2", time.Minute) // 覆盖
	v, ok = s.Get("k")
	require.True(t, ok)
	assert.Equal(t, "v2", v)
	assert.Equal(t, 1, s.Len())
}

// TestGet_TTLExpiry 时钟注入验证过期（惰性删除）。
func TestGet_TTLExpiry(t *testing.T) {
	now := time.Now()
	s := New().WithClock(func() time.Time { return now })

	s.Set("k", "v", time.Second)
	_, ok := s.Get("k")
	require.True(t, ok)

	now = now.Add(2 * time.Second)
	_, ok = s.Get("k")
	assert.False(t, ok, "过期后应未命中")
	assert.Equal(t, 0, s.Len(), "过期条目应被惰性删除")
}

// TestSetNX_Atomic 不存在才写入；过期条目可被 SetNX 覆写。
func TestSetNX_Atomic(t *testing.T) {
	now := time.Now()
	s := New().WithClock(func() time.Time { return now })

	assert.True(t, s.SetNX("k", "v1", time.Minute), "首次写入成功")
	assert.False(t, s.SetNX("k", "v2", time.Minute), "已存在拒绝")
	v, _ := s.Get("k")
	assert.Equal(t, "v1", v, "拒绝时不得覆盖")

	now = now.Add(2 * time.Minute)
	assert.True(t, s.SetNX("k", "v3", time.Minute), "过期条目视为不存在")
	v, _ = s.Get("k")
	assert.Equal(t, "v3", v)
}

// TestSetNX_Concurrent 并发 SetNX 同键仅一人成功（NX 唯一性语义）。
func TestSetNX_Concurrent(t *testing.T) {
	s := New()
	const n = 50
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		winners  int
		loserVal string
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if s.SetNX("race", "w"+string(rune('a'+i)), time.Minute) {
				mu.Lock()
				winners++
				loserVal = "w" + string(rune('a'+i))
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	assert.Equal(t, 1, winners, "并发 NX 只允许一个写入者")
	v, _ := s.Get("race")
	assert.Equal(t, loserVal, v)
}

// TestDel 返回实际删除数，未命中计 0。
func TestDel(t *testing.T) {
	s := New()
	s.Set("a", "1", time.Minute)
	s.Set("b", "2", time.Minute)
	assert.Equal(t, 2, s.Del("a", "b", "c"))
	assert.Equal(t, 0, s.Len())
	assert.Equal(t, 0, s.Del("a"))
}

// TestCapacityFIFO_Eviction 满载守卫（收编 oekit LocalCache 后的口径）：写满
// DefaultMax 后插入新键触发 **FIFO 逐出**——最早写入的键让位，Len 恒 ≤ DefaultMax。
//
// 旧口径是「先清扫过期、仍满则整表重置」；差异与理由见包文档「满载语义」：
// 防 DoS 的内存上界不变，被撑爆时的附带伤害从「殃及全部键」收窄为「逐出最旧键」。
func TestCapacityFIFO_Eviction(t *testing.T) {
	now := time.Now()
	s := New().WithClock(func() time.Time { return now })

	for i := 0; i < DefaultMax; i++ {
		s.Set("k"+strconv.Itoa(i), "v", time.Hour)
	}
	require.Equal(t, DefaultMax, s.Len())

	s.Set("overflow", "v", time.Hour) // 满载插入 ⇒ FIFO 逐出最旧
	assert.Equal(t, DefaultMax, s.Len(), "满载不得增长（内存上界）")

	_, ok := s.Get("k0")
	assert.False(t, ok, "最早写入的键应被逐出")

	v, ok := s.Get("overflow")
	require.True(t, ok, "新键应存活")
	assert.Equal(t, "v", v)

	_, ok = s.Get("k1")
	assert.True(t, ok, "只逐出最旧一格，次旧键应存活")
}

// TestZeroTTL_NoExpiry ttl<=0 = 永不过期。
func TestZeroTTL_NoExpiry(t *testing.T) {
	now := time.Now()
	s := New().WithClock(func() time.Time { return now })
	s.Set("k", "v", 0)
	now = now.Add(100 * time.Hour)
	v, ok := s.Get("k")
	require.True(t, ok)
	assert.Equal(t, "v", v)
}
