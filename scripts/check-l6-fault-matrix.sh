#!/usr/bin/env bash
# L6 故障注入测试层矩阵门禁：清单（pkg/l6/matrix.json）↔ 仓内真实测试 **双向对账**。
#
# 为什么要有这道门禁（L6 此前是「零散用例」，没有系统化矩阵）：
#   · 清单里的每条用例必须挂着仓内**真实存在**的测试（AST 级核对，不是字符串 grep）；
#   · 仓内每个故障注入测试文件必须登记进清单（没登记 ⇒ 新用例游离在矩阵外）；
#   · 五类故障每类的用例数不低于清单里声明的下限（防止「某类故障悄悄没覆盖」）；
#   · 期望语义与业务码必须在受控词表内（写错类别名、自造语义都跑不掉）。
#
# 分工（两层，读同一份清单，判据不重复实现）：
#   · 本脚本 = 入口层：清单在场性 + 汇总打印 + --selfcheck 负对照；
#   · pkg/l6/matrix_guard_test.go = 判定层（编译进 CI，core 的 go test ./... 自动跑）。
#
# 用法：
#   bash scripts/check-l6-fault-matrix.sh              # 跑门禁
#   bash scripts/check-l6-fault-matrix.sh --selfcheck  # 负对照：注入坏形态必须判红
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT" || exit 1

MANIFEST="pkg/l6/matrix.json"
GUARD_PKG="./pkg/l6/"
GUARD_TEST="TestL6MatrixGuard"

SELFCHECK=0
for arg in "$@"; do
  case "$arg" in
    --selfcheck) SELFCHECK=1 ;;
    *)
      echo "用法: $0 [--selfcheck]" >&2
      exit 2
      ;;
  esac
done

log() { printf '%s\n' "$*"; }
fail() {
  printf '✗ %s\n' "$*" >&2
  exit 1
}

WORKDIR=""
# shellcheck disable=SC2329 # cleanup 由 trap 触发（不是直接调用）
cleanup() {
  rc=$?
  if [ -n "$WORKDIR" ] && [ -d "$WORKDIR" ]; then
    rm -rf "$WORKDIR"
  fi
  return "$rc"
}
trap cleanup EXIT

# build_guard 编译判定层（一次编译，多次复用——selfcheck 要跑多形态）。
build_guard() {
  WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/pb-l6-gate.XXXXXX")" || fail "无法创建临时目录"
  GUARD_BIN="$WORKDIR/l6guard.test"
  go test -c -o "$GUARD_BIN" "$GUARD_PKG" ||
    fail "编译判定层失败（$GUARD_PKG）"
}

# run_guard <清单路径> <日志路径>：跑判定层，返回退出码。
run_guard() {
  manifest=$1
  logfile=$2
  PB_L6_MATRIX="$manifest" "$GUARD_BIN" -test.run "$GUARD_TEST" -test.v >"$logfile" 2>&1
  return $?
}

# ── 正常形态 ────────────────────────────────────────────────────────────
if [ "$SELFCHECK" -eq 0 ]; then
  [ -f "$MANIFEST" ] || fail "清单不存在：$MANIFEST（先补 pkg/l6/matrix.json）"
  [ -s "$MANIFEST" ] || fail "清单为空：$MANIFEST"

  build_guard
  run_guard "$MANIFEST" "$WORKDIR/main.log"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    log "判定层输出："
    sed 's/^/  /' "$WORKDIR/main.log" >&2
    fail "L6 矩阵对账未通过（清单 ↔ 仓内测试不一致）"
  fi

  log "✓ L6 故障注入测试层矩阵对账通过"
  grep -E 'L6 矩阵对账通过' "$WORKDIR/main.log" | sed 's/^.*matrix_guard_test.go:[0-9]*: //;s/^/  /'
  exit 0
fi

# ── 负对照（--selfcheck）────────────────────────────────────────────────
# 纪律（与 L8 门禁、hub 的 check-upgrade-recoverability.sh 同口径）：
#   坏形态必须判红（判据有牙齿），好形态必须放行；
#   任何一种形态的判定与预期不符 ⇒ selfcheck 自身判红。
log "L6 矩阵门禁自检（注入坏形态 ⇒ 必须判红；注入好形态 ⇒ 必须放行）"

command -v python3 >/dev/null 2>&1 ||
  fail "python3 不可用：负对照需要它注入坏形态清单（macOS/Linux 自带）"

build_guard

# mutate <源清单> <目标清单> <形态编号>：用 python3 就地改写清单制造坏形态。
mutate() {
  src=$1
  dst=$2
  form=$3
  python3 - "$src" "$dst" "$form" <<'PY'
import json
import sys

src, dst, form = sys.argv[1], sys.argv[2], sys.argv[3]
with open(src, encoding="utf-8") as fh:
    m = json.load(fh)

if form == "bad-func":
    # 坏形态①：清单引用一个并不存在的测试函数（正向对账必须判红）
    m["cases"][0]["tests"] = ["app/share/l6_storage_fault_test.go::TestL6_ThisFunctionDoesNotExist"]
elif form == "bad-kind":
    # 坏形态②：写出词表外的故障类别名（类别值域必须封闭）
    m["cases"][0]["kind"] = "disk_full_but_selfmade"
elif form == "bad-mode":
    # 坏形态③：写出词表外的期望语义（自造语义必须判红）
    m["cases"][0]["expect"]["modes"] = ["do_something_weird"]
elif form == "bad-threshold":
    # 坏形态④：把某类的覆盖下限抬到实际条数之上（覆盖下限必须生效）
    for k in m["kinds"]:
        if k["id"] == "redis_down":
            k["min_cases"] = 999
elif form == "unregistered-file":
    # 坏形态⑤：删掉所有引用某故障注入文件的用例（该文件变成未登记）
    target = "app/share/l6_storage_fault_test.go"
    m["cases"] = [c for c in m["cases"]
                  if not any(t.startswith(target + "::") for t in c["tests"])]
elif form == "typed-error-no-code":
    # 坏形态⑥：期望语义含 typed_error 却不声明业务码（码与语义必须配套）
    for c in m["cases"]:
        if "typed_error" in c["expect"]["modes"]:
            c["expect"]["code"] = 0
            break
    else:
        # 清单里没有 typed_error 用例时，改为在第一条用例上强行加上（不带码）
        m["cases"][0]["expect"]["modes"] = ["typed_error"]
        m["cases"][0]["expect"]["code"] = 0
elif form == "bad-json":
    with open(dst, "w", encoding="utf-8") as fh:
        fh.write("{ this is not valid json\n")
    sys.exit(0)
else:
    sys.exit("未知形态: " + form)

with open(dst, "w", encoding="utf-8") as fh:
    json.dump(m, fh, ensure_ascii=False, indent=2)
PY
}

sc_rc=0

# expect_red <形态名> <清单路径>：该形态必须判红。
expect_red() {
  form=$1
  manifest=$2
  run_guard "$manifest" "$WORKDIR/$form.log"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    log "  ✗ $form：坏形态被放行了（判据没有牙齿）"
    sc_rc=1
    return 1
  fi
  log "  ✓ $form：坏形态被判红（退出码 $rc）"
  return 0
}

# expect_green <形态名> <清单路径>：该形态必须放行。
expect_green() {
  form=$1
  manifest=$2
  run_guard "$manifest" "$WORKDIR/$form.log"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    log "  ✗ $form：好形态被误判红（退出码 $rc）"
    sed 's/^/      /' "$WORKDIR/$form.log" | head -20
    sc_rc=1
    return 1
  fi
  log "  ✓ $form：好形态放行（退出码 0）"
  return 0
}

# 好形态：原样清单必须放行。
expect_green "好形态（原样清单）" "$MANIFEST"

# 坏形态逐个注入：每个都必须判红。
for form in bad-func bad-kind bad-mode bad-threshold unregistered-file typed-error-no-code bad-json; do
  mutate "$MANIFEST" "$WORKDIR/$form.json" "$form"
  expect_red "$form" "$WORKDIR/$form.json"
done

if [ "$sc_rc" -ne 0 ]; then
  fail "L6 矩阵门禁自检未通过：至少一种形态的判定与预期不符"
fi
log "✓ L6 矩阵门禁自检通过（7 种坏形态全部判红 + 好形态放行）"
exit 0
