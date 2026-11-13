#!/usr/bin/env bash
# Record the fastvm terminal demo with VHS.
#
# One command from a clean checkout to a finished GIF: starts a throwaway
# fastvmd (root, or an unprivileged user namespace so sudo is not required),
# builds the demo base disk from hack/Dockerfile.example, then runs VHS from the
# repository root so the tape's relative `Output docs/demo.gif` lands in docs/.
#
# Usage: hack/demo-vhs.sh [tape]   (default: docs/demo.tape in the repository root)
#        KEEP=1 hack/demo-vhs.sh   (keep the daemon and /tmp state for debugging)
#
# Requires: vhs (Chrome + ffmpeg + ttyd), firecracker, docker, KVM, and root or
# unprivileged user namespaces (fastvmd needs CAP_NET_ADMIN for TAP/nftables).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ORIG_PWD="$PWD"; TAPE="${1:-docs/demo.tape}"
cd "$ROOT"
if [[ "$TAPE" != /* ]]; then
  if [[ -f "$ORIG_PWD/$TAPE" ]]; then TAPE="$ORIG_PWD/$TAPE"; else TAPE="$ROOT/$TAPE"; fi
fi

fail() { echo "error: $*" >&2; exit 1; }

# --- Preflight: fail with an actionable message instead of a cryptic mid-demo error.
command -v vhs >/dev/null || fail "vhs not found; install VHS: https://github.com/charmbracelet/vhs"
for bin in bin/fastvm bin/fastvmd bin/fastvm-agent; do
  [[ -x "$bin" ]] || fail "$bin missing; run 'make build'"
done
[[ -f test/e2e/assets/vmlinux.bin ]] || fail "test/e2e/assets/vmlinux.bin missing; run 'make assets'"
command -v firecracker >/dev/null || fail "firecracker not found; install Firecracker v1.17+"
command -v docker >/dev/null || fail "docker not found; it is required to build the demo image"
docker info >/dev/null 2>&1 || fail "docker is not usable; start the docker daemon and check user $(id -un) can access it"
[[ -f "$TAPE" ]] || fail "tape not found: $TAPE (the default is docs/demo.tape in the repo root)"

if [[ "$(id -u)" -eq 0 ]]; then
  MODE="root"
elif unshare -Urn --map-root-user true 2>/dev/null; then
  MODE="user namespace (unshare -Urn --map-root-user)"
else
  fail "needs root or unprivileged user namespaces: fastvmd requires CAP_NET_ADMIN for TAP/bridge/nftables; run with sudo, or enable unprivileged user namespaces"
fi

# --- Throwaway state plus a config derived from hack/config.yaml (file untouched).
WORK="$(mktemp -d /tmp/fastvm-vhs.XXXXXX)"
DATA_DIR="$WORK/data"
SOCKET="$WORK/fastvm.sock"
LOG="$WORK/daemon.log"
PIDFILE="$WORK/daemon.pid"
DAEMON_CONFIG="$WORK/config.yaml"
mkdir -p "$DATA_DIR"
# Absolute kernel/agent paths keep the config valid wherever the daemon starts;
# data_dir/socket_path are overridden on the daemon command line below.
sed -e "s|^kernel_path:.*|kernel_path: $ROOT/test/e2e/assets/vmlinux.bin|" \
    -e "s|^agent_path:.*|agent_path: $ROOT/bin/fastvm-agent|" \
    hack/config.yaml > "$DAEMON_CONFIG"

# Resolve the tape's Output path the way VHS will: relative to the repo root.
GIF="$(sed -n 's/^[[:space:]]*Output[[:space:]]\{1,\}//p' "$TAPE" | head -n 1 | sed 's/[[:space:]]*$//')"
GIF="${GIF%\"}"; GIF="${GIF#\"}"
[[ -n "$GIF" ]] || GIF="docs/demo.gif"
if [[ "$GIF" != /* ]]; then GIF="$ROOT/$GIF"; fi

still_alive() { local p; for p in "$@"; do kill -0 "$p" 2>/dev/null && return 0; done; return 1; }

CLEANED=0; INNER_PID=""; DAEMON_PIDS=()
cleanup() {
  [[ "$CLEANED" -eq 1 ]] && return 0
  CLEANED=1
  if [[ "${KEEP:-0}" == "1" ]]; then
    echo
    echo "KEEP=1: leaving $WORK in place for debugging."
    if (( ${#DAEMON_PIDS[@]} > 0 )); then echo "  stop the daemon: kill ${DAEMON_PIDS[*]}"; fi
    echo "  remove the state: rm -rf $WORK"
    return 0
  fi
  if (( ${#DAEMON_PIDS[@]} > 0 )); then
    for p in "${DAEMON_PIDS[@]}"; do kill -TERM "$p" 2>/dev/null || true; done
    for _ in $(seq 1 100); do
      still_alive "${DAEMON_PIDS[@]}" || break
      sleep 0.1
    done
    for p in "${DAEMON_PIDS[@]}"; do kill -KILL "$p" 2>/dev/null || true; done
    wait "$DAEMON_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
  return 0
}
trap cleanup EXIT
trap 'exit 130' INT; trap 'exit 143' TERM

echo "== starting fastvmd ($MODE; state: $WORK) =="
DAEMON_CMD=("$ROOT/bin/fastvmd" -config "$DAEMON_CONFIG" -data-dir "$DATA_DIR" -socket "$SOCKET" -log-level info)
if [[ "$(id -u)" -eq 0 ]]; then
  "${DAEMON_CMD[@]}" > "$LOG" 2>&1 &
  DAEMON_PID=$!
else
  # `exec` makes fastvmd inherit the PID recorded in PIDFILE. $! is usually the
  # daemon itself; recording the inner PID also covers util-linux versions that
  # keep a wrapper process around.
  unshare -Urn --map-root-user sh -c 'ip link set lo up; echo $$ > "$1"; shift; exec "$@"' \
    fastvm-vhs-daemon "$PIDFILE" "${DAEMON_CMD[@]}" > "$LOG" 2>&1 &
  DAEMON_PID=$!
fi
DAEMON_PIDS=("$DAEMON_PID")
for _ in $(seq 1 150); do
  [[ -S "$SOCKET" ]] && break
  kill -0 "$DAEMON_PID" 2>/dev/null || break
  sleep 0.1
done
INNER_PID="$(cat "$PIDFILE" 2>/dev/null || true)"
if [[ -n "$INNER_PID" && "$INNER_PID" != "$DAEMON_PID" ]]; then DAEMON_PIDS+=("$INNER_PID"); fi
if [[ ! -S "$SOCKET" ]]; then
  echo "error: fastvmd did not create $SOCKET within 15s (log: $LOG)" >&2
  tail -n 20 "$LOG" 2>/dev/null >&2 || true
  exit 1
fi
echo "   daemon pid $DAEMON_PID, socket $SOCKET"

echo; echo "== building demo:latest from hack/Dockerfile.example (cached busybox; first run may take a while) =="
if ! FASTVM_SOCKET="$SOCKET" bin/fastvm image build -f hack/Dockerfile.example -t demo:latest hack; then
  echo "error: building demo:latest failed (docker access?); rerun with KEEP=1 to inspect the daemon log" >&2
  exit 1
fi

echo; echo "== recording $TAPE with VHS -> $GIF =="
START="$(date +%s)"; RC=0
FASTVM_SOCKET="$SOCKET" vhs "$TAPE" || RC=$?
if [[ "$RC" -ne 0 ]]; then
  echo "error: vhs failed (exit $RC); no GIF was recorded (output above)" >&2
  exit "$RC"
fi
DURATION="$(( $(date +%s) - START ))"
[[ -s "$GIF" ]] || fail "vhs exited 0 but $GIF is missing or empty; check the tape's 'Output' line"

echo; echo "== done: $GIF ($(du -h "$GIF" | cut -f1)) recorded in ${DURATION}s =="
echo "   tape:  $TAPE"
echo "   env:   FASTVM_SOCKET=$SOCKET (fastvmd mode: $MODE)"
echo "   rerun: hack/demo-vhs.sh ${1:-docs/demo.tape}"
if [[ "${KEEP:-0}" == "1" ]]; then echo "   daemon and $WORK kept (KEEP=1); see stop instructions below"; else echo "   daemon stopped and $WORK removed on exit (KEEP=1 to preserve them)"; fi
