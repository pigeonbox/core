package model

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// 审计 target 的列宽是硬约束（PG/MySQL 写不进、换库导入拒导）。
// 写入方按常量封顶 ⇒ 常量与结构体 tag 的 size 必须一致，否则又来一次
// 「按 A 封顶、按 B 建列」的漂移（写入方以为装得下、实际装不下）。
func TestAdminOperationLogTargetSizeMatchesConstant(t *testing.T) {
	f, ok := reflect.TypeOf(AdminOperationLog{}).FieldByName("Target")
	if !ok {
		t.Fatal("AdminOperationLog 缺 Target 字段")
	}
	tag := f.Tag.Get("gorm")
	want := "size:" + strconv.Itoa(AdminOperationLogTargetMaxLen)
	if !strings.Contains(tag, want) {
		t.Fatalf("Target 的 gorm tag = %q，未含 %q（列宽与常量漂移：写入方按常量封顶，实际列宽不同）", tag, want)
	}
}
