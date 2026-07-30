#!/usr/bin/env bash
# Optimistic-concurrency demo: a status write based on a stale resourceVersion
# is rejected with 409 Conflict, and the correct reaction is re-read + retry.
#
# This reproduces, with kubectl, exactly what the agent does in code:
#   snapshot   → internal/agent/kube.go  Get()
#   stale PUT  → internal/agent/kube.go  PutStatus()   (409 → ErrConflict)
#   recovery   → internal/agent/kube.go  UpdateStatus() (re-read, re-apply)
set -euo pipefail

DEV=conflict-demo-dev
NS=default
say() { printf '\n\033[1;36m>>> %s\033[0m\n' "$*"; }

say "1. Create device (spec v1.0.0) — this is the state the device last saw"
kubectl apply -f - <<EOF >/dev/null
apiVersion: edge.example.com/v1alpha1
kind: EdgeDevice
metadata: {name: $DEV, namespace: $NS}
spec:
  desiredFirmwareVersion: "1.0.0"
  firmwareURL: "http://127.0.0.1:8000/firmware-1.0.0.bin"
  checksumSHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
EOF

say "2. Device 'goes offline' holding a snapshot of the object"
SNAPSHOT=$(kubectl -n $NS get edgedevice $DEV -o json)
STALE_RV=$(echo "$SNAPSHOT" | python3 -c 'import sys,json; print(json.load(sys.stdin)["metadata"]["resourceVersion"])')
echo "    snapshot resourceVersion = $STALE_RV"

say "3. While it is away, the spec changes TWICE (rv moves on without it)"
kubectl -n $NS patch edgedevice $DEV --type merge -p '{"spec":{"desiredFirmwareVersion":"2.0.0"}}' >/dev/null
kubectl -n $NS patch edgedevice $DEV --type merge -p '{"spec":{"desiredFirmwareVersion":"3.0.0"}}' >/dev/null
NOW_RV=$(kubectl -n $NS get edgedevice $DEV -o jsonpath='{.metadata.resourceVersion}')
echo "    latest resourceVersion   = $NOW_RV (device still holds $STALE_RV)"

say "4. Device comes back and writes status FROM ITS STALE SNAPSHOT"
STALE_WRITE=$(echo "$SNAPSHOT" | python3 -c '
import sys, json
o = json.load(sys.stdin)
o.setdefault("status", {})["currentFirmwareVersion"] = "1.0.0"
json.dump(o, sys.stdout)')
if echo "$STALE_WRITE" | kubectl -n $NS replace --subresource=status -f - 2>/tmp/conflict-err.txt; then
  echo "UNEXPECTED: stale write was accepted"; exit 1
else
  echo "    API server said:"
  sed 's/^/      /' /tmp/conflict-err.txt
fi

say "5. Conflict is not an error — it is the signal that the latest state wins."
say "   Recovery = discard stale view, GET fresh, re-apply, write succeeds:"
kubectl -n $NS get edgedevice $DEV -o json | python3 -c '
import sys, json
o = json.load(sys.stdin)
o.setdefault("status", {})["currentFirmwareVersion"] = "1.0.0"
json.dump(o, sys.stdout)' | kubectl -n $NS replace --subresource=status -f - >/dev/null
echo "    status written against fresh resourceVersion — accepted"

say "6. And the re-read brought back the news the device missed:"
kubectl -n $NS get edgedevice $DEV -o custom-columns=NAME:.metadata.name,DESIRED:.spec.desiredFirmwareVersion,CURRENT:.status.currentFirmwareVersion
echo "
    The device now sees desired=3.0.0 (not the intermediate 2.0.0 — latest
    intent wins) and its state machine starts a new upgrade transaction.
    In the agent this whole dance is internal/agent/kube.go UpdateStatus()."

kubectl -n $NS delete edgedevice $DEV --wait=false >/dev/null 2>&1 || true
