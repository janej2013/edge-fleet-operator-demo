#!/usr/bin/env bash
# Ensure the kind cluster is up and healthy, recreating it if necessary.
# kind clusters do not reliably survive Docker Desktop's resource-saver
# suspend/resume (control-plane container exits 128 on resume), so "ensure"
# means: probe, and rebuild from scratch when the probe fails. Rebuild is
# cheap (~30s) because everything in the cluster is declarative anyway —
# which is itself a nice property to point at.
set -euo pipefail

CLUSTER=${CLUSTER:-edge-fleet}
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  if kubectl --context "kind-$CLUSTER" get nodes >/dev/null 2>&1; then
    echo "kind cluster '$CLUSTER' is healthy"
  else
    echo "kind cluster '$CLUSTER' exists but is unreachable — recreating"
    kind delete cluster --name "$CLUSTER"
    kind create cluster --name "$CLUSTER" --wait 60s
  fi
else
  kind create cluster --name "$CLUSTER" --wait 60s
fi

kubectl config use-context "kind-$CLUSTER" >/dev/null
make -C "$ROOT" install >/dev/null
echo "CRDs installed"
