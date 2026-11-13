#!/usr/bin/env bash
# fastvm wake-on-network demo.
#
# Uses hack/config.yaml, but shortens its auto-sleep windows (idle 3s, pause 2s,
# offload 8s) so the demo does not wait minutes: build a base disk, boot a
# sandbox with a published port, let it auto-sleep, then wake it with curl.
#
# Requires root (TAP/bridge/nftables) and docker (to build the demo image).
# No system-wide installation or file editing is needed.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

CONFIG="$ROOT/hack/config.yaml"
WORK="$(mktemp -d /tmp/fastvm-demo.XXXXXX)"
DATA_DIR="$WORK/data"
SOCKET="$WORK/fastvm.sock"
LOG="$WORK/daemon.log"
DEMO_CONFIG="$WORK/demo-config.yaml"

if [[ "$(id -u)" -ne 0 ]]; then
  echo "error: run as root (fastvmd needs CAP_NET_ADMIN for TAP/bridge/nftables)" >&2
  echo "       e.g. sudo make demo" >&2
  exit 1
fi
for bin in bin/fastvmd bin/fastvm bin/fastvm-agent; do
  [[ -x "$bin" ]] || { echo "error: $bin missing; run 'make build'" >&2; exit 1; }
done
[[ -f test/e2e/assets/vmlinux.bin ]] || { echo "error: kernel missing; run 'make assets'" >&2; exit 1; }
command -v docker >/dev/null || { echo "error: docker is required to build the demo image" >&2; exit 1; }
command -v curl >/dev/null || { echo "error: curl is required" >&2; exit 1; }

export FASTVM_SOCKET="$SOCKET"

mkdir -p "$DATA_DIR"

# The checked-in config uses generous windows; shorten them for the demo.
sed -e 's/^  idle_window:.*/  idle_window: 3s/' \
    -e 's/^  pause_after:.*/  pause_after: 2s/' \
    -e 's/^  offload_after:.*/  offload_after: 8s/' \
    "$CONFIG" > "$DEMO_CONFIG"

cleanup() {
  if [[ -n "${DAEMON:-}" ]] && kill -0 "$DAEMON" 2>/dev/null; then
    kill "$DAEMON" 2>/dev/null || true
    wait "$DAEMON" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "== starting fastvmd with hack/config.yaml (shortened auto-sleep: 3s/2s/8s) =="
bin/fastvmd -config "$DEMO_CONFIG" -data-dir "$DATA_DIR" -socket "$SOCKET" -log-level info > "$LOG" 2>&1 &
DAEMON=$!
for _ in $(seq 1 100); do [[ -S "$SOCKET" ]] && break; sleep 0.1; done
if [[ ! -S "$SOCKET" ]]; then
  echo "error: fastvmd did not create $SOCKET" >&2
  cat "$LOG" >&2 || true
  exit 1
fi
echo "   daemon pid $DAEMON, socket $SOCKET, data $DATA_DIR (removed on exit)"

echo
echo "== 1. build a base disk from a Dockerfile =="
bin/fastvm image build -f "$ROOT/hack/Dockerfile.example" -t demo:latest "$ROOT/hack"

echo
echo "== 2. boot a sandbox and publish port 8080 -> guest 80 =="
bin/fastvm run --image demo:latest --name web --publish 8080:80 -- \
  /bin/sh -c "echo hello-from-sleeping-vm > /tmp/index.html; exec /bin/busybox httpd -f -p 0.0.0.0:80 -h /tmp"

echo
echo "== 3. curl while the sandbox is running =="
for _ in $(seq 1 20); do
  if curl -fsS --max-time 2 http://127.0.0.1:8080/; then break; fi
  sleep 0.3
done

state() { bin/fastvm get web | sed -n 's/.*"state": "\([^"]*\)".*/\1/p' | head -1; }
wake_count() { bin/fastvm get web | sed -n 's/.*"wake_count": \([0-9]*\).*/\1/p' | head -1; }

echo
echo "== 4. wait for auto-sleep (idle -> paused -> dormant) =="
for _ in $(seq 1 60); do
  [[ "$(state)" == "Dormant" ]] && break
  sleep 1
done
echo "   state: $(state)  wake_count: $(wake_count)"

echo
echo "== 5. curl the sleeping sandbox: the packet wakes it, then it answers =="
/usr/bin/time -f "   curl wall: %e s" curl -fsS --max-time 20 http://127.0.0.1:8080/
echo "   state: $(state)  wake_count: $(wake_count)"

echo
echo "== 6. lifecycle events =="
bin/fastvm events -n 8

echo
echo "demo complete. daemon stopped; demo state removed."
