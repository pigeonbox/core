#!/usr/bin/env bash
# L3 集成层：S3 兼容**真后端**门禁（起真服务端 → 跑 core 的 S3 驱动集成用例）。
#
# 为什么用 RustFS 而不是 MinIO 容器（2026-11-18 实测，不是猜测）：
#   MinIO 官方三条分发路径现已**不可匿名获取**：
#     · Docker Hub `minio/minio:<tag>` → 「pull access denied for minio/minio,
#       repository does not exist or may require 'docker login'」；
#     · quay.io/minio/minio:<tag>      → 匿名取 manifest 401 UNAUTHORIZED
#       （连 `latest` 与 2024 年旧 tag 也一样 ⇒ 是仓库级鉴权，不是 tag 缺失）；
#     · dl.min.io/server/minio/...     → 410 Gone（官方归档已清空）。
#   ⇒ 继续钉在 MinIO 容器上，这一步只会恒红（假红即噪声），而恒红等于没门禁。
#
#   RustFS = MinIO 兼容的真实 S3 服务端（Rust 实现，MinIO 的 drop-in 替代）。
#   它的 GitHub Release 资产带**官方 sha256**（GitHub API 的 asset.digest 字段）
#   ⇒ 可完全钉版、不碰镜像仓库、不依赖匿名拉取权限。
#   **被测面不变**：core 的 S3 驱动走 minio-go（S3 协议面），
#   RustFS 与 MinIO 同协议 ⇒ 覆盖的是同一层（PUT/GET/DELETE/覆盖写/分片合并/
#   列表/预签名直传直下/故障面）。
#   任何人若有真实 MinIO 端点，直接给 PB_TEST_MINIO_ENDPOINT 等 env 即可跑同一批用例。
#
# 纪律（与本仓「真机不可达 ⇒ 红」同款，见 scripts/live-storage-gate.sh）：
#   · 摘要未钉版 ⇒ 红（打印实际摘要供回填；「没钉版」不许看起来像「钉过了」）；
#   · 摘要与钉版不符 ⇒ 红；
#   · 资产下载失败 / 解压失败 ⇒ 红；
#   · 服务起不来或不就绪 ⇒ 红；
#   · 用例走 SKIP 分支 ⇒ 红（grep 断言没跳过）；
#   · 用例没跑 ⇒ 红（断言 PASS 记录存在）。
#
# 用法：
#   scripts/l3-s3-backend.sh              # 起服务 + 跑用例 + 收尾
#   scripts/l3-s3-backend.sh --selfcheck  # 负对照自检（纯本地、不联网）
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

# ── 钉版参数（仓内真值；改这里等于改供应链钉版）──
PB_L3_S3_VERSION="${PB_L3_S3_VERSION:-1.0.6}"
PB_L3_S3_ASSET="${PB_L3_S3_ASSET:-rustfs-linux-x86_64-gnu-v1.0.6.zip}"
PB_L3_S3_SHA256="${PB_L3_S3_SHA256:-4f0bf3fecbe861467427ff506aee22c302cfd5ac8b7dcbde71714f8e5e680dea}"
PB_L3_S3_PORT="${PB_L3_S3_PORT:-9000}"
PB_L3_S3_BASE_URL="${PB_L3_S3_BASE_URL:-https://github.com/rustfs/rustfs/releases/download}"

# 用例侧 env（core 的 S3 驱动门控，见 storage/minio_integration_test.go）
PB_TEST_MINIO_ENDPOINT="${PB_TEST_MINIO_ENDPOINT:-127.0.0.1:${PB_L3_S3_PORT}}"
PB_TEST_MINIO_ACCESS_KEY="${PB_TEST_MINIO_ACCESS_KEY:-pbminio}"
PB_TEST_MINIO_SECRET_KEY="${PB_TEST_MINIO_SECRET_KEY:-pbminio123}"
PB_TEST_MINIO_BUCKET="${PB_TEST_MINIO_BUCKET:-pigeonbox-l3-test}"
export PB_TEST_MINIO_ENDPOINT PB_TEST_MINIO_ACCESS_KEY PB_TEST_MINIO_SECRET_KEY PB_TEST_MINIO_BUCKET
export PB_REQUIRE_REMOTE_STORAGE="${PB_REQUIRE_REMOTE_STORAGE:-1}"

log() { printf '=== %s\n' "$*"; }
fail() {
  printf '✗ %s\n' "$*" >&2
  exit 1
}

# sha256_of <file> → 打印小写 hex（兼容 sha256sum / shasum 两种环境）
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# verify_digest <file> <pin>：钉版核对（空 pin ⇒ bootstrap 判红；不符 ⇒ 判红）
verify_digest() {
  _file="$1"
  _pin="$2"
  _actual="$(sha256_of "$_file")"
  if [ -z "$_pin" ]; then
    printf '✗ 摘要尚未钉版（bootstrap）：实际=sha256:%s\n' "$_actual" >&2
    return 1
  fi
  if [ "$_actual" != "$_pin" ]; then
    printf '✗ 摘要与钉版不符：实际=sha256:%s（期望 %s）\n' "$_actual" "$_pin" >&2
    return 1
  fi
  printf '✓ 摘要核对通过：sha256:%s\n' "$_actual"
  return 0
}

# assert_no_skip <logfile>：服务已就绪 ⇒ 用例不许走 SKIP 分支
assert_no_skip() {
  if grep -q '^--- SKIP:' "$1"; then
    printf '✗ 真机用例被 SKIP（服务已就绪却跳过 ⇒ 判据没真正执行）\n' >&2
    return 1
  fi
  return 0
}

# assert_has_pass <logfile>：用例必须留下 PASS 记录
assert_has_pass() {
  if ! grep -qE '^--- PASS: TestMinIOIntegration' "$1"; then
    printf '✗ 未发现真机用例的执行记录（判据没被执行）\n' >&2
    return 1
  fi
  return 0
}

# wait_ready <url> <attempts>：健康探针（不复用 curl 静默失败即成功的口径）
wait_ready() {
  _url="$1"
  _n=0
  _max="$2"
  while [ "$_n" -lt "$_max" ]; do
    _n=$((_n + 1))
    if curl -fsS -o /dev/null "$_url" 2>/dev/null; then
      return 0
    fi
    sleep 2
  done
  return 1
}

# ── 负对照自检（纯本地、不联网）──
selfcheck() {
  log "L3 后端门禁：负对照自检"
  _tmp="$(mktemp -d /tmp/pb-l3-selfcheck.XXXXXX)"
  _probe="$_tmp/probe"
  printf 'pigeonbox-l3-selfcheck' >"$_probe"
  _real="$(sha256_of "$_probe")"

  verify_digest "$_probe" "$_real" >/dev/null 2>&1 || { fail "负对照①：正确摘要应当放行，却判红"; }
  verify_digest "$_probe" "0000000000000000000000000000000000000000000000000000000000000000" >/dev/null 2>&1 && fail "负对照②：错误摘要必须判红，却放行了"
  verify_digest "$_probe" "" >/dev/null 2>&1 && fail "负对照③：未钉版（空 pin）必须判红，却放行了"

  _log_ok="$_tmp/ok.log"
  printf -- '--- PASS: TestMinIOIntegrationLifecycle (0.1s)\n' >"$_log_ok"
  _log_skip="$_tmp/skip.log"
  printf -- '--- SKIP: MinIO 集成 env 未配置\n' >"$_log_skip"

  assert_no_skip "$_log_ok" || fail "负对照④：无 SKIP 的日志应当放行，却判红"
  assert_has_pass "$_log_ok" || fail "负对照⑤：有 PASS 记录的日志应当放行，却判红"
  assert_no_skip "$_log_skip" && fail "负对照⑥：含 SKIP 的日志必须判红，却放行了"
  assert_has_pass "$_log_skip" && fail "负对照⑦：无 PASS 记录的日志必须判红，却放行了"

  wait_ready "http://127.0.0.1:1/minio/health/live" 2 >/dev/null 2>&1 && fail "负对照⑧：服务不就绪必须判红，却放行了"

  rm -rf "$_tmp"
  log "✓ 负对照自检全绿（8 项：钉版放行/不符/未钉版、SKIP 放行/判红、PASS 放行/判红、未就绪判红）"
}

if [ "${1:-}" = "--selfcheck" ]; then
  selfcheck
  exit 0
fi

# ── 主流程 ──
WORK_DIR="$(mktemp -d /tmp/pb-l3-s3.XXXXXX)"
SRV_PID=""
cleanup() {
  if [ -n "$SRV_PID" ]; then
    kill "$SRV_PID" >/dev/null 2>&1 || true
    wait "$SRV_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK_DIR" >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "下载 RustFS ${PB_L3_S3_VERSION} 资产（钉版 sha256）"
URL="${PB_L3_S3_BASE_URL}/${PB_L3_S3_VERSION}/${PB_L3_S3_ASSET}"
ZIP="$WORK_DIR/${PB_L3_S3_ASSET}"
if ! curl -fsSL --retry 5 --retry-delay 3 --retry-all-errors \
  --connect-timeout 30 --max-time 900 -o "$ZIP" "$URL"; then
  fail "资产下载失败：${URL}"
fi
verify_digest "$ZIP" "$PB_L3_S3_SHA256" || fail "供应链钉版核对未过"

log "解压"
mkdir -p "$WORK_DIR/bin"
if ! unzip -q -o "$ZIP" -d "$WORK_DIR/bin"; then
  fail "解压失败：${PB_L3_S3_ASSET}"
fi
SRV_BIN="$(find "$WORK_DIR/bin" -type f -name 'rustfs' | head -n 1)"
[ -n "$SRV_BIN" ] || fail "解压后未找到 rustfs 可执行文件"
chmod +x "$SRV_BIN"

log "起 S3 真后端（RustFS）"
DATA_DIR="$WORK_DIR/data"
mkdir -p "$DATA_DIR"
RUSTFS_ACCESS_KEY="$PB_TEST_MINIO_ACCESS_KEY" \
  RUSTFS_SECRET_KEY="$PB_TEST_MINIO_SECRET_KEY" \
  RUSTFS_VOLUMES="$DATA_DIR" \
  RUSTFS_ADDRESS="127.0.0.1:${PB_L3_S3_PORT}" \
  RUSTFS_CONSOLE_ENABLE=false \
  "$SRV_BIN" server "$DATA_DIR" >"$WORK_DIR/server.log" 2>&1 &
SRV_PID=$!

log "等待就绪…"
HEALTH_URL="http://127.0.0.1:${PB_L3_S3_PORT}/minio/health/live"
if ! wait_ready "$HEALTH_URL" 60; then
  tail -20 "$WORK_DIR/server.log" >&2 || true
  fail "S3 真后端未就绪（${HEALTH_URL}）"
fi
log "✓ S3 真后端就绪"

log "跑 S3 驱动集成用例"
go test ./storage/ -run 'TestMinIOIntegration' -v -count=1 2>&1 | tee "$WORK_DIR/test.log"
rc=${PIPESTATUS[0]}

assert_no_skip "$WORK_DIR/test.log" || fail "用例被 SKIP ⇒ 判据没真正执行"
assert_has_pass "$WORK_DIR/test.log" || fail "用例没跑 ⇒ 判据没被执行"
[ "$rc" = "0" ] || fail "用例失败（rc=$rc）"

log "✓ L3 S3 真后端验收通过"
