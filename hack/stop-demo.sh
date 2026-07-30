#!/usr/bin/env bash
# stop-demo: kill demo processes and delete demo CRs. Deletion happens while
# the operator may already be dead, so stuck finalizers are cleared manually.
set -uo pipefail
source "$(dirname "$0")/demo-lib.sh"

purge_demo_crs || true
stop_all_processes
echo "demo processes stopped, demo CRs removed"
