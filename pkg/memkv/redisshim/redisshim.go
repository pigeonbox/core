// Package redisshim 单机内存模式下 go-redis 命令面的等价实现（memkv.Store 的
// redis 兼容垫片）：redis.host 为空时 anonymous/presign 等域的 NewService 以此
// 兜底，取件码映射/直传会话等短 TTL 数据存进程内——不跨进程共享、重启即失，
// DB 仍是唯一真相源。容量/清扫参数用 memkv 包默认值（上限+周期清扫，对齐
// lockout 内存兜底先例）。
//
// 命令结果用 go-redis 的 New*Result 构造器包装，redis.Nil 表不存在——
// 消费域既有的 redis.Nil 判断两种模式通用，无分叉。
//
// 独立成子包是把 redis 客户端依赖隔离在 memkv 主包之外（本包依赖 go-redis）。
// 历史：anonymous/presign 曾各持一份几乎相同的 memoryKV（且 SetArgs 只在
// anonymous 侧实现），2026-10-10 合并至此，命令面取两侧超集。
package redisshim

import (
	"context"
	"fmt"
	"time"

	"github.com/pigeonbox/core/pkg/memkv"
	"github.com/redis/go-redis/v9"
)

// KV memkv.Store 的 go-redis 命令面包装。Store 字段导出，供消费域测试
// 直接断言底层键值（如负缓存标记/映射回填）。
type KV struct {
	Store *memkv.Store
}

// New 构造独立实例（每个消费域自持一份，不共享表）。
func New() KV {
	return KV{Store: memkv.New()}
}

// toStr 归一化写入值：消费面传 string（映射/展示信息）与 []byte（meta JSON），
// 其余兜底格式化。
func toStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(v)
	}
}

func (m KV) Get(_ context.Context, key string) *redis.StringCmd {
	if v, ok := m.Store.Get(key); ok {
		return redis.NewStringResult(v, nil)
	}
	return redis.NewStringResult("", redis.Nil)
}

func (m KV) Set(_ context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd {
	m.Store.Set(key, toStr(value), expiration)
	return redis.NewStatusResult("OK", nil)
}

// SetArgs 消费面仅用 Mode:"NX"+TTL（GenerateCode 唯一性），其余选项不支持（fail-loud）。
func (m KV) SetArgs(_ context.Context, key string, value any, a redis.SetArgs) *redis.StatusCmd {
	switch a.Mode {
	case "NX":
		if !m.Store.SetNX(key, toStr(value), a.TTL) {
			// 已存在：与 Redis NX 未命中语义一致（服务层按 redis.Nil 重试换码）
			return redis.NewStatusResult("", redis.Nil)
		}
		return redis.NewStatusResult("OK", nil)
	case "":
		m.Store.Set(key, toStr(value), a.TTL)
		return redis.NewStatusResult("OK", nil)
	default:
		return redis.NewStatusResult("", fmt.Errorf("redisshim: unsupported SetArgs mode %q", a.Mode))
	}
}

func (m KV) Del(_ context.Context, keys ...string) *redis.IntCmd {
	return redis.NewIntResult(int64(m.Store.Del(keys...)), nil)
}
