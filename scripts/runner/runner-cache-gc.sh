#!/usr/bin/env bash
# Proactive disk-fill cap for self-hosted runners. ISI-3489 (root cause of ISI-3475).
#
# The reactive runner-watchdog disk branch (>=90% -> prune) was firing ~11x/day
# because nothing bounded the Go build cache: GOCACHE grew to ~17G on gitrunner
# and docker layers accumulated, so the disk climbed back to 90% within ~10 min
# under CI load. This timer keeps the disk BELOW the reactive threshold so the
# watchdog's disk branch (and any deferred restart) almost never has to fire.
#
# Non-destructive to live jobs: docker prune only touches dangling / >24h-old
# layers (never in-use images), and the GOCACHE trim is skipped entirely while a
# Runner.Worker job is executing so a running `go test` never loses its cache.
set -euo pipefail

GOCACHE_DIR="${GOCACHE_DIR:-/home/ubuntu/.cache/go-build}"
GOCACHE_MAX_GB="${GOCACHE_MAX_GB:-8}"

job_live() { pgrep -f "Runner.Worker" >/dev/null 2>&1; }

# Always safe, even mid-job: dangling images and build cache older than 24h are
# not the running job's working set.
docker image prune -af --filter until=24h >/dev/null 2>&1 || true
docker builder prune -af --filter until=24h >/dev/null 2>&1 || true

if job_live; then
  logger -t runner-cache-gc "job in progress; skipped GOCACHE trim (ISI-3489)"
  exit 0
fi

sz_gb=$(du -sBG "$GOCACHE_DIR" 2>/dev/null | awk '{print $1+0}')
if [ "${sz_gb:-0}" -ge "$GOCACHE_MAX_GB" ]; then
  # `go clean -cache` respects the GOCACHE env override; fall back to rm if the
  # go toolchain is not on PATH for the timer's user.
  if command -v go >/dev/null 2>&1; then
    GOCACHE="$GOCACHE_DIR" go clean -cache 2>/dev/null || rm -rf "${GOCACHE_DIR:?}/"* 2>/dev/null || true
  else
    rm -rf "${GOCACHE_DIR:?}/"* 2>/dev/null || true
  fi
  logger -t runner-cache-gc "GOCACHE was ${sz_gb}G >= ${GOCACHE_MAX_GB}G; cleaned (idle)"
fi
