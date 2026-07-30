#!/usr/bin/env bash
# Shared plumbing for the demo scripts. Everything stateful lives under
# ~/.edge-fleet on the WSL/Linux filesystem — NOT the repo dir — because the
# agent's A/B slots need real symlinks (drvfs mounts like /mnt/d don't
# reliably support them) and because demo state should never end up in git.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE="$HOME/.edge-fleet"
BIN="$STATE/bin"
FW_DIR="$STATE/firmware"
LOG_DIR="$STATE/logs"
PID_DIR="$STATE/pids"
KUBECONFIG_FILE="$STATE/kubeconfig"

mkdir -p "$BIN" "$FW_DIR" "$LOG_DIR" "$PID_DIR"

say()  { printf '\n\033[1;36m>>> %s\033[0m\n' "$*"; }
note() { printf '\033[0;33m    %s\033[0m\n' "$*"; }

build_bins() {
  say "Building agent, firmware-server, operator"
  (cd "$ROOT" && go build -o "$BIN/agent" ./cmd/agent \
              && go build -o "$BIN/firmware-server" ./cmd/firmware-server \
              && go build -o "$BIN/operator" ./cmd)
}

export_kubeconfig() {
  kind get kubeconfig --name edge-fleet > "$KUBECONFIG_FILE"
}

# make_firmware <version> — creates a random "image" and prints its sha256.
make_firmware() {
  local f="$FW_DIR/firmware-$1.bin"
  [ -f "$f" ] || head -c 262144 /dev/urandom > "$f"
  sha256sum "$f" | cut -d' ' -f1
}

start_firmware_server() {
  pkill -x firmware-server 2>/dev/null || true
  nohup "$BIN/firmware-server" -dir "$FW_DIR" -addr :8000 > "$LOG_DIR/firmware-server.log" 2>&1 &
  echo $! > "$PID_DIR/firmware-server.pid"
  disown
}

# start_operator <heartbeat-timeout>
start_operator() {
  pkill -x operator 2>/dev/null || true
  nohup "$BIN/operator" --heartbeat-timeout="$1" \
    --metrics-bind-address=:8080 --metrics-secure=false \
    --health-probe-bind-address=:8081 \
    > "$LOG_DIR/operator.log" 2>&1 &
  echo $! > "$PID_DIR/operator.pid"
  disown
}

# create_device <name> <version> <checksum>
create_device() {
  kubectl apply -f - <<EOF >/dev/null
apiVersion: edge.example.com/v1alpha1
kind: EdgeDevice
metadata:
  name: $1
  namespace: default
  labels: {fleet: demo}
spec:
  desiredFirmwareVersion: "$2"
  firmwareURL: "http://127.0.0.1:8000/firmware-$2.bin"
  checksumSHA256: "$3"
EOF
}

# start_agent <name>
start_agent() {
  rm -rf "$STATE/devices/$1"
  nohup "$BIN/agent" -name "$1" -kubeconfig "$KUBECONFIG_FILE" \
    -data-dir "$STATE/devices/$1" -poll-interval 2s -heartbeat-interval 2s \
    > "$LOG_DIR/$1.log" 2>&1 &
  echo $! > "$PID_DIR/$1.pid"
  disown
}

kill_agent() {
  kill -9 "$(cat "$PID_DIR/$1.pid")" 2>/dev/null || true
  rm -f "$PID_DIR/$1.pid"
}

stop_all_processes() {
  for p in "$PID_DIR"/*.pid; do
    [ -f "$p" ] && kill -9 "$(cat "$p")" 2>/dev/null || true
    rm -f "$p"
  done
  pkill -x firmware-server 2>/dev/null || true
  pkill -x operator 2>/dev/null || true
}

# purge_demo_crs: remove ALL demo CRs, releasing finalizers by hand (the
# operator that would normally run cleanup may not be alive at this point).
purge_demo_crs() {
  kubectl delete fleetrollouts --all --wait=false >/dev/null 2>&1 || true
  kubectl delete edgedevices --all --wait=false >/dev/null 2>&1 || true
  sleep 1
  for d in $(kubectl get edgedevices -o name 2>/dev/null); do
    kubectl patch "$d" --type merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
  done
  local deadline=$((SECONDS + 30))
  while [ -n "$(kubectl get edgedevices,fleetrollouts -o name 2>/dev/null)" ]; do
    if [ $SECONDS -ge $deadline ]; then echo "failed to purge demo CRs" >&2; return 1; fi
    sleep 1
  done
}

# wait_until <description> <timeout-seconds> <command...>
# Polls until the command succeeds; dumps diagnostics and fails on timeout.
wait_until() {
  local desc=$1 timeout=$2; shift 2
  local deadline=$((SECONDS + timeout))
  until "$@" >/dev/null 2>&1; do
    if [ $SECONDS -ge $deadline ]; then
      echo "TIMEOUT waiting for: $desc" >&2
      kubectl get edgedevices,fleetrollouts 2>&1 || true
      return 1
    fi
    sleep 1
  done
  note "$desc"
}

# device_at <name> <version> — true when status.currentFirmwareVersion matches.
device_at() {
  [ "$(kubectl get edgedevice "$1" -o jsonpath='{.status.currentFirmwareVersion}' 2>/dev/null)" = "$2" ]
}

rollout_phase_is() {
  [ "$(kubectl get fleetrollout "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" = "$2" ]
}
