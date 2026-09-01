#!/usr/bin/env bash
# Render self-check for the control-plane NATS/JetStream bus + event-relay wiring
# (ISI-3506). Asserts the fail-closed + auto-derive behavior that the default CI
# `helm template` gate (controlPlane.enabled=false) never exercises. No framework:
# runnable as `config/helm/ci/nats-render-test.sh` from anywhere.
set -euo pipefail

CHART="$(cd "$(dirname "$0")/.." && pwd)"
DSN="--set controlPlane.enabled=true --set controlPlane.database.dsn=postgres://x"
fail() { echo "FAIL: $1" >&2; exit 1; }

# 1. Default (control plane off): the bus must NOT render and must NOT fail.
out="$(helm template t "$CHART" 2>&1)" || fail "default render errored"
echo "$out" | grep -q "kind: StatefulSet" && fail "NATS rendered while controlPlane.enabled=false"

# 2. Plane on + NATS on but no StorageClass: fail-closed (§16.2, no cluster default).
if helm template t "$CHART" $DSN >/dev/null 2>&1; then
  fail "missing controlPlane.nats.storage.storageClassName should fail-closed"
fi

# 3. Full plane: NATS StatefulSet renders and event-relay auto-derives the URL.
out="$(helm template t "$CHART" $DSN --set controlPlane.nats.storage.storageClassName=csi-nfs 2>&1)" \
  || fail "full-plane render errored"
echo "$out" | grep -q "kind: StatefulSet" || fail "NATS StatefulSet missing in full plane"
echo "$out" | grep -q 'value: "nats://ksquad-nats.k8squad-system.svc:4222"' \
  || fail "event-relay RELAY_NATS_URL not auto-derived from in-cluster service"
echo "$out" | grep -q "max_file_store: 4GB" || fail "jetstream max_file_store must be emitted UNQUOTED"

# 4. NATS off + relay on + no external URL: fail-closed (relay hard-requires a bus).
if helm template t "$CHART" $DSN --set controlPlane.nats.enabled=false >/dev/null 2>&1; then
  fail "event-relay with no bus should fail-closed"
fi

# 5. NATS off + relay off: storage-free plane still renders clean.
helm template t "$CHART" $DSN \
  --set controlPlane.nats.enabled=false --set controlPlane.eventRelay.enabled=false >/dev/null \
  || fail "storage-free plane (bus disabled) should render"

echo "OK: control-plane NATS/event-relay render self-check passed"
