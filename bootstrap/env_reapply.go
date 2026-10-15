package bootstrap

// env 恒覆盖重施器（2026-10-15 统一配置优先级）。
//
// 统一规则：yaml < DB(system_configs) < env（env 非空即恒覆盖）。
// 此前 SystemConfig 托管段（ui/upload_ex/download/notify/local_import/oidc/
// api_token/p2p 联邦）的 DB overlay 会整段盖掉启动时 viper 绑定的 env 值
// ——env 只在首次入库前生效，与 storage 的 env>DB 规则相悖，管理员
// "改了后台配置重启后回退/不回退"取决于段别。本文件让全部托管段对齐：
// 每次 DB overlay 之后（启动 RestoreAdminSettings、后台保存、多副本广播
// 恢复），凡 env 绑定键非空者把 env 值重新施回全局 conf。
//
// DB 仍保留管理员的提交意图（system_configs 不动），仅运行态以 env 为准；
// 内存镜像经 syncMirrorFromGlobalLocked 回同步，管理后台读到恒为生效值。

import (
	"fmt"
	"log"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/pigeonbox/core/conf"
)

// ReapplyEnvOverrides 把非空 env 绑定值重新施加到全局 conf（幂等）。
// g 由调用方注入（bootstrap 包级 config / OnOverlayApplied 闭包捕获），
// 不直读 conf.GetGlobalConfig（棘轮规则 8：装配层也不再新增直读面）。
// 由 config service 的 OnOverlayApplied 钩子在每次 DB overlay 后调用；
// 启动期因钩子注册晚于首次 RestoreAdminSettings，wiring 在注册后显式补调一次。
func ReapplyEnvOverrides(g *conf.AppConfiguration) {
	if g == nil {
		return
	}
	root := reflect.ValueOf(g).Elem()
	for key, envs := range envBindings {
		val := ""
		for _, env := range envs {
			if v := os.Getenv(env); v != "" {
				val = v
				break
			}
		}
		if val == "" {
			continue
		}
		if err := setConfPath(root, key, val); err != nil {
			// 单键失败不阻断：配置必须兜底可启动，问题留在日志
			log.Printf("[env-reapply] skip %s (%s=…): %v", key, envs[0], err)
		}
	}
}

// setConfPath 按 "a.b.c" 点径沿 mapstructure tag 走结构体并给叶字段赋值。
func setConfPath(root reflect.Value, path, raw string) error {
	parts := strings.Split(path, ".")
	cur := root
	for i, p := range parts {
		if cur.Kind() == reflect.Ptr {
			if cur.IsNil() {
				return fmt.Errorf("nil segment at %q", strings.Join(parts[:i], "."))
			}
			cur = cur.Elem()
		}
		if cur.Kind() != reflect.Struct {
			return fmt.Errorf("%q is not a struct", strings.Join(parts[:i+1], "."))
		}
		f := findConfField(cur, p)
		if !f.IsValid() {
			return fmt.Errorf("field %q not found", p)
		}
		if i == len(parts)-1 {
			return setConfLeaf(f, raw)
		}
		cur = f
	}
	return nil
}

// findConfField 按 mapstructure tag（逗号前主名）匹配导出字段。
func findConfField(structVal reflect.Value, tag string) reflect.Value {
	t := structVal.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		name := strings.Split(f.Tag.Get("mapstructure"), ",")[0]
		if name == tag {
			return structVal.Field(i)
		}
	}
	return reflect.Value{}
}

// setConfLeaf 把 env 字符串按叶字段类型赋值（指针叶自动分配元素；
// 字符串切片叶按 CSV 拆分——envBindings 里的列表键全部走 splitCSV 语义）。
func setConfLeaf(field reflect.Value, raw string) error {
	t := field.Type()
	for t.Kind() == reflect.Ptr {
		if field.IsNil() {
			field.Set(reflect.New(t.Elem()))
		}
		field = field.Elem()
		t = field.Type()
	}
	switch t.Kind() {
	case reflect.String:
		field.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("bool parse: %w", err)
		}
		field.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, t.Bits())
		if err != nil {
			return fmt.Errorf("int parse: %w", err)
		}
		field.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, t.Bits())
		if err != nil {
			return fmt.Errorf("uint parse: %w", err)
		}
		field.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(raw, t.Bits())
		if err != nil {
			return fmt.Errorf("float parse: %w", err)
		}
		field.SetFloat(f)
	case reflect.Slice:
		if t.Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported slice elem %s", t.Elem().Kind())
		}
		field.Set(reflect.ValueOf(splitCSV(raw)))
	default:
		return fmt.Errorf("unsupported kind %s", t.Kind())
	}
	return nil
}
