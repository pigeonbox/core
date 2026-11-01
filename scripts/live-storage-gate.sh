#!/usr/bin/env bash
# 存储驱动真机门禁：起**真服务**（Azurite + HDFS WebHDFS）并跑 live 用例。
#
# 用途：core#36 的验收门禁（此前 Azure Blob / HDFS 驱动只有单测/mock，
# 真机三个缺陷零阻力溜过 ⇒ 单测全绿而真机全线不可用）。
#
# ⚠️ 与「跑测试」的区别：**真服务不可达 ⇒ 红**（不是跳过）。
#    跳过 = 假绿 = 真机验收形同虚设（同 core 仓「换库真库验收」步的纪律：
#    "容器没起来 ⇒ **红**（不是跳过）"）。
#
# 用法：
#   scripts/live-storage-gate.sh          # 起服务 + 跑用例 + 收尾
#   scripts/live-storage-gate.sh --keep   # 跑完保留容器（调试用）
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

AZURITE_PORT="${PB_LIVE_AZURE_BLOB_PORT:-10000}"
HDFS_PORT="${PB_LIVE_HDFS_PORT:-9870}"
HDFS_DATA_PORT="${PB_LIVE_HDFS_DATANODE_PORT:-9864}"
HDFS_RPC_PORT="${PB_LIVE_HDFS_RPC_PORT:-8020}"
AZURITE_DIR="$(mktemp -d /tmp/pb-azurite.XXXXXX)"

log() { printf '=== %s\n' "$*"; }
fail() { printf '✗ %s\n' "$*" >&2; exit 1; }

cleanup() {
  log "收尾"
  if [ "$KEEP" = "1" ]; then
    log "--keep：保留 pb-live-hdfs / Azurite"
    return
  fi
  docker rm -f pb-live-hdfs >/dev/null 2>&1 || true
  # 精确 kill 本脚本起的 Azurite（不 pkill 模糊匹配 ⇒ 不误杀他人进程）
  if [ -n "${az_pid:-}" ]; then
    kill "$az_pid" >/dev/null 2>&1 || true
  fi
  rm -rf "$AZURITE_DIR" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ── 前置：Docker 可用 ──
command -v docker >/dev/null 2>&1 || fail "docker 不可用 ⇒ 真机门禁无法进行"
docker info >/dev/null 2>&1 || fail "docker daemon 不可用 ⇒ 真机门禁无法进行"

# ── 1) 起 HDFS（真 WebHDFS）──
log "起 HDFS（apache/hadoop 单节点）"
docker rm -f pb-live-hdfs >/dev/null 2>&1 || true
docker run -d --name pb-live-hdfs --platform linux/amd64 \
  -p "${HDFS_PORT}:9870" -p "${HDFS_DATA_PORT}:9864" -p "${HDFS_RPC_PORT}:8020" \
  apache/hadoop:3.3.6 sleep infinity >/dev/null 2>&1 || fail "HDFS 容器启动失败"
docker cp "$REPO_ROOT/scripts/live-storage-services.sh" pb-live-hdfs:/setup.sh >/dev/null 2>&1 \
  || fail "复制 hdfs 配置脚本失败"
docker exec pb-live-hdfs bash /setup.sh >/tmp/pb-hdfs-setup.log 2>&1 \
  || { cat /tmp/pb-hdfs-setup.log; fail "HDFS 初始化失败（见 /tmp/pb-hdfs-setup.log）"; }

hdfs_ready=0
for _ in $(seq 1 90); do
  code="$(curl -s -o /dev/null -w '%{http_code}' \
    "http://127.0.0.1:${HDFS_PORT}/webhdfs/v1/?op=LISTSTATUS" 2>/dev/null)"
  case "$code" in 200|403) hdfs_ready=1; break ;; esac
  sleep 2
done
[ "$hdfs_ready" = "1" ] || { docker logs pb-live-hdfs 2>&1 | tail -20; fail "WebHDFS 未就绪（端口 ${HDFS_PORT}）"; }
log "✓ HDFS 就绪（:${HDFS_PORT}）"

# ── 2) 起 Azurite（真 Azure Blob REST）──
log "起 Azurite（Azure Blob 模拟器，blobPort=${AZURITE_PORT}）"
if command -v npx >/dev/null 2>&1; then
  npx --yes azurite --silent --location "$AZURITE_DIR" \
    --blobPort "$AZURITE_PORT" --queuePort "$((AZURITE_PORT + 1))" --tablePort "$((AZURITE_PORT + 2))" \
    >/tmp/pb-azurite.log 2>&1 &
  # 后台 PID 供 cleanup 精确 kill（不靠 pkill 模糊匹配，免误杀他人进程）
  az_pid=$!
  export az_pid
else
  fail "npx 不可用 ⇒ 无法起 Azurite"
fi

az_ready=0
for _ in $(seq 1 60); do
  code="$(curl -s -o /dev/null -w '%{http_code}' \
    "http://127.0.0.1:${AZURITE_PORT}/devstoreaccount1?restype=container&comp=list" 2>/dev/null)"
  case "$code" in 200|403|404) az_ready=1; break ;; esac
  sleep 1
done
[ "$az_ready" = "1" ] || { cat /tmp/pb-azurite.log 2>/dev/null | tail -20; fail "Azurite 未就绪（端口 ${AZURITE_PORT}）"; }
log "✓ Azurite 就绪（:${AZURITE_PORT}）"

# ── 3) 跑真机用例 ──
log "跑 live 用例（真服务）"
export PB_LIVE_AZURE_ENDPOINT="http://127.0.0.1:${AZURITE_PORT}/devstoreaccount1"
export PB_LIVE_HDFS_ENDPOINT="http://127.0.0.1:${HDFS_PORT}"
export PB_LIVE_HDFS_USER="root"

GOWORK=off go test -tags live ./storage/opendal/ -run 'TestLiveAzureBlob|TestLiveHDFS' -v -count=1 2>&1 | tee /tmp/pb-live-storage.log
rc=${PIPESTATUS[0]}

# ── 4) 「跳过」不许冒充「通过」──
# 真服务已就绪 ⇒ 用例**不得**走 SKIP 分支（SKIP 说明它压根没连上真机）。
if grep -q '^--- SKIP:' /tmp/pb-live-storage.log; then
  fail "真机用例被 SKIP（服务已就绪却跳过 ⇒ 判据没真正执行）"
fi
if grep -qE '^--- (PASS|FAIL): (TestLiveAzureBlob|TestLiveHDFS)' /tmp/pb-live-storage.log; then
  :
else
  fail "未发现真机用例的执行记录（判据没被执行）"
fi

[ "$rc" = "0" ] || fail "真机用例失败（rc=${rc}）"
log "✓ 真机门禁通过（Azure Blob + HDFS）"
