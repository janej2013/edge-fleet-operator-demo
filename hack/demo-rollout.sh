#!/usr/bin/env bash
# demo-rollout: 5 simulated devices upgrade 1.0.0 → 2.0.0 in batches of 2.
# After batch 1 lands, one device is killed mid-campaign: the operator marks
# it Degraded via heartbeat timeout, the rollout counts it as failed, and the
# circuit breaker freezes the fleet.
set -euo pipefail
source "$(dirname "$0")/demo-lib.sh"

HEARTBEAT_TIMEOUT=12s
DEVICES=5

build_bins
export_kubeconfig
stop_all_processes
purge_demo_crs

say "Preparing firmware images (1.0.0 baseline, 2.0.0 rollout target)"
SUM1=$(make_firmware 1.0.0)
SUM2=$(make_firmware 2.0.0)
start_firmware_server
start_operator "$HEARTBEAT_TIMEOUT"

say "Provisioning $DEVICES devices at 1.0.0 and starting their agents"
for i in $(seq 0 $((DEVICES-1))); do
  create_device "dev-$i" 1.0.0 "$SUM1"
  start_agent "dev-$i"
done
for i in $(seq 0 $((DEVICES-1))); do
  wait_until "dev-$i is running 1.0.0" 60 device_at "dev-$i" 1.0.0
done
kubectl get edgedevices

say "Killing dev-3's agent — the device dies just as the campaign begins"
kill_agent dev-3
note "its heartbeat stops now; the operator will notice after $HEARTBEAT_TIMEOUT"

say "Starting rollout to 2.0.0: maxUnavailable=2, breaker threshold=20%"
kubectl apply -f - <<EOF >/dev/null
apiVersion: edge.example.com/v1alpha1
kind: FleetRollout
metadata: {name: demo-rollout, namespace: default}
spec:
  selector: {fleet: demo}
  targetVersion: "2.0.0"
  firmwareURL: "http://127.0.0.1:8000/firmware-2.0.0.bin"
  checksumSHA256: "$SUM2"
  maxUnavailable: 2
  failureThresholdPercent: 20
EOF

wait_until "batch 1 (dev-0, dev-1) upgraded to 2.0.0" 90 \
  bash -c "$(declare -f device_at); device_at dev-0 2.0.0 && device_at dev-1 2.0.0"
kubectl get fleetrollout demo-rollout
note "batches keep advancing while dev-3's silence is still within the heartbeat budget..."

say "Waiting for heartbeat timeout → Degraded → circuit breaker"
wait_until "circuit breaker tripped (rollout Paused)" 120 rollout_phase_is demo-rollout Paused

say "Final fleet state:"
kubectl get fleetrollout demo-rollout
kubectl get edgedevices
note "dev-3 is Degraded (agent dead, heartbeat expired) and counted as failed"
note "every device now has spec.rolloutPaused=true — the breaker froze the fleet"
note "the breaker never self-resets: resuming is a human decision"

say "Operator log — the breaker firing:"
grep -E "Degraded|circuit breaker" "$LOG_DIR/operator.log" | tail -5 || true

say "Agent log (dev-1) — one full upgrade transaction:"
grep -E "state transition|upgrade" "$LOG_DIR/dev-1.log" | tail -10 || true

stop_all_processes
say "Done. Logs remain in $LOG_DIR, device state in $STATE/devices"
