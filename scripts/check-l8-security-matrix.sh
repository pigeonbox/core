#!/usr/bin/env bash
# L8 安全测试层矩阵门禁：清单（pkg/l8/matrix.json）↔ 仓内真实测试 **双向对账**。
#
# 为什么要有这道门禁（L8 此前是"零散用例"，没有系统化矩阵）：
#   · 清单里的每条用例必须挂着仓内**真实存在**的测试（AST 级核对，不是字符串 grep）；
#   · 仓内每个安全测试文件/函数必须登记进清单（没登记 ⇒ 新安全用例游离在矩阵外）；
#   · 六个威胁类每类的用例数不低于清单里声明的下限（防止"某类威胁悄悄没覆盖"）。
#
# 分工（两层，读同一份清单，判据不重复实现）：
#   · 本脚本 = 入口层：清单在场性 + 汇总打印 + --selfcheck 负对照；
#   · pkg/l8/matrix_guard_test.go = 判定层（编译进 CI，core 的 go test ./... 自动跑）。
#
# 用法：
#   bash scripts/check-l8-security-matrix.sh              # 跑门禁
#   bash scripts/check-l8-security-matrix.sh --selfcheck  # 负对照：注入坏形态必须判红
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT" || exit 1

MANIFEST="pkg/l8/matrix.json"
GUARD_PKG="./pkg/l8/"
GUARD_TEST="TestL8MatrixGuard"

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

log() { printf '%s
' "$*"; }
fail() {
  printf '✗ %s
' "$*" >&2
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
  WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/pb-l8-gate.XXXXXX")" || fail "无法创建临时目录"
  GUARD_BIN="$WORKDIR/l8guard.test"
  go test -c -o "$GUARD_BIN" "$GUARD_PKG" ||
    fail "编译判定层失败（$GUARD_PKG）"
}

# run_guard <清单路径> <日志路径>：跑判定层，返回退出码。
run_guard() {
  manifest=$1
  logfile=$2
  PB_L8_MATRIX="$manifest" "$GUARD_BIN" -test.run "$GUARD_TEST" -test.v >"$logfile" 2>&1
  return $?
}

# ── 正常形态 ────────────────────────────────────────────────────────────
if [ "$SELFCHECK" -eq 0 ]; then
  [ -f "$MANIFEST" ] || fail "清单不存在：$MANIFEST（先补 pkg/l8/matrix.json）"
  [ -s "$MANIFEST" ] || fail "清单为空：$MANIFEST"

  build_guard
  run_guard "$MANIFEST" "$WORKDIR/main.log"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    log "判定层输出："
    sed 's/^/  /' "$WORKDIR/main.log" >&2
    fail "L8 矩阵对账未通过（清单 ↔ 仓内测试不一致）"
  fi

  log "✓ L8 安全测试层矩阵对账通过"
  grep -E 'L8 矩阵对账通过|用例 +[0-9]+ 条' "$WORKDIR/main.log" | sed 's/^.*matrix_guard_test.go:[0-9]*: //;s/^/  /'
  exit 0
fi

# ── 负对照（--selfcheck）────────────────────────────────────────────────
# 纪律（与 hub 的 check-upgrade-recoverability.sh 同口径）：
#   坏形态必须判红（判据有牙齿），好形态必须放行；
#   任何一种形态的判定与预期不符 ⇒ selfcheck 自身判红。
log "L8 矩阵门禁自检（注入坏形态 ⇒ 必须判红；注入好形态 ⇒ 必须放行）"

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
    m["cases"][0]["tests"] = ["pkg/auth/l8_jwt_algorithm_test.go::TestL8_ThisFunctionDoesNotExist"]
elif form == "bad-class":
    # 坏形态②：威胁类缺失一个（六个主键必须齐备）
    m["classes"] = [c for c in m["classes"] if c["id"] != "ssrf"]
elif form == "bad-threshold":
    # 坏形态③：把某类的覆盖下限抬到实际条数之上（覆盖下限必须生效）
    for c in m["classes"]:
        if c["id"] == "auth_bypass":
            c["min_cases"] = 999
elif form == "unregistered-file":
    # 坏形态④：删掉所有引用某安全测试文件的用例（该文件变成未登记）
    target = "pkg/auth/jwt_test.go"
    m["cases"] = [c for c in m["cases"]
                  if not any(t.startswith(target + "::") for t in c["tests"])]
elif form == "unregistered-func":
    # 坏形态⑤：从用例里摘掉一条测试（该函数既没被认领也没被豁免）
    target = "pkg/auth/jwt_test.go::TestRefreshToken_Invalid"
    for c in m["cases"]:
        if target in c["tests"]:
            c["tests"] = [t for t in c["tests"] if t != target]
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
for form in bad-func bad-class bad-threshold unregistered-file unregistered-func bad-json; do
  mutate "$MANIFEST" "$WORKDIR/$form.json" "$form"
  expect_red "$form" "$WORKDIR/$form.json"
done

if [ "$sc_rc" -ne 0 ]; then
  fail "L8 矩阵门禁自检未通过：至少一种形态的判定与预期不符"
fi
log "✓ L8 矩阵门禁自检通过（6 种坏形态全部判红 + 好形态放行）"
exit 0
