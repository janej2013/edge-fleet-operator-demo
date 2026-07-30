#!/usr/bin/env bash
# run-agents <N>: standing playground — N devices at 1.0.0 with live agents.
# Combine with `make run-operator` in another terminal and poke Specs by hand.
set -euo pipefail
source "$(dirname "$0")/demo-lib.sh"

N=${1:-5}
build_bins
export_kubeconfig
SUM1=$(make_firmware 1.0.0)
start_firmware_server

say "Starting $N agents (logs: $LOG_DIR/dev-*.log)"
for i in $(seq 0 $((N-1))); do
  create_device "dev-$i" 1.0.0 "$SUM1"
  start_agent "dev-$i"
done
kubectl get edgedevices
note "stop everything with: make stop-demo"
