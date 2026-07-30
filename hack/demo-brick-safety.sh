#!/usr/bin/env bash
# demo-brick-safety: push an update whose checksum doesn't match the bytes.
# The agent downloads, verification fails, and the A/B switch NEVER happens —
# the device keeps running the old firmware instead of bricking.
set -euo pipefail
source "$(dirname "$0")/demo-lib.sh"

build_bins
export_kubeconfig
stop_all_processes
purge_demo_crs

SUM1=$(make_firmware 1.0.0)
SUM2=$(make_firmware 2.0.0)
start_firmware_server
start_operator 30s

say "Device starts healthy at 1.0.0"
create_device dev-brick 1.0.0 "$SUM1"
start_agent dev-brick
wait_until "dev-brick is running 1.0.0" 60 device_at dev-brick 1.0.0

say "Pushing 2.0.0 with a WRONG checksum (bytes won't match — corrupted image)"
kubectl patch edgedevice dev-brick --type merge -p "{
  \"spec\": {
    \"desiredFirmwareVersion\": \"2.0.0\",
    \"firmwareURL\": \"http://127.0.0.1:8000/firmware-2.0.0.bin\",
    \"checksumSHA256\": \"$SUM1\"
  }
}" >/dev/null
note "(spec says 2.0.0 but carries 1.0.0's checksum — verification must fail)"

wait_until "agent reported ChecksumMismatch" 60 bash -c \
  'kubectl get edgedevice dev-brick -o jsonpath="{.status.conditions[*].reason}" | grep -q ChecksumMismatch'

say "Agent log — download OK, verification failed, NO switch:"
grep -E "state transition|upgrade" "$LOG_DIR/dev-brick.log" | tail -6

say "The device is NOT bricked:"
kubectl get edgedevice dev-brick
echo "    active slot symlink: $(readlink "$STATE/devices/dev-brick/current") (unchanged)"
echo "    running version:     $(cat "$STATE/devices/dev-brick/$(readlink "$STATE/devices/dev-brick/current")/version")"
note "Phase is Degraded (a live device with a failed transaction), and the"
note "poisoned target is not retried until the Spec changes — fix the"
note "checksum and the agent immediately picks it up."

stop_all_processes
