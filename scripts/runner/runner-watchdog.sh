#!/usr/bin/env bash
# Self-heal the self-hosted Actions runner. ISI-2577 (origin) + ISI-3475/ISI-3489 (hardening).
#
# Handles three failure modes:
#   1. disk-full crash-loop  (>=90% -> prune space)
#   2. network-flap / half-connected listener (stuck retrying -> restart)
#   3. hard systemd failure  (unit == failed -> reset-failed + restart)
#
# ISI-3475: the disk branch used to run an UNCONDITIONAL `systemctl restart`,
# which cancels any in-flight CI job ("runner has received a shutdown signal").
# gitrunner-2 refilled to >=90% every ~10 min under CI load, so this fired
# ~11x/day and rolling-killed long go/operator + go/apiserver jobs. The restart
# is now deferred while a Runner.Worker job is live.
#
# ISI-3489: the reactive `rm -rf go-build/*` prune is also gated on idle so it
# cannot wipe the build cache out from under a running `go test`. Docker/diag
# pruning stays unconditional (it frees space without touching a live job).
# Proactive disk-fill prevention lives in runner-cache-gc.sh — this script is the
# last-resort reactive guard; steady state it should almost never take the
# disk branch.
set -euo pipefail

# Auto-detect the runner unit so this script is host-agnostic (gitrunner,
# gitrunner-2, future runners). Falls back to $SVC if the caller pins one.
SVC="${SVC:-$(systemctl list-units 'actions.runner.*' --plain --no-legend --all 2>/dev/null \
  | awk '{print $1}' | grep -m1 '\.service$' || true)}"
if [ -z "${SVC:-}" ]; then
  logger -t runner-watchdog "no actions.runner.* unit found; nothing to guard"
  exit 0
fi

GOCACHE_DIR="${GOCACHE_DIR:-/home/ubuntu/.cache/go-build}"
DIAG_DIR="${DIAG_DIR:-/home/ubuntu/actions-runner/_diag}"
STALE_SECS="${STALE_SECS:-600}"
now=$(date +%s)

job_live() { pgrep -f "Runner.Worker" >/dev/null 2>&1; }

usep=$(df --output=pcent / | tail -1 | tr -dc 0-9)
if [ "${usep:-0}" -ge 90 ]; then
  # Always safe, even mid-job: dangling images/volumes and stale diag logs are
  # not owned by the running job.
  docker system prune -af --filter until=48h >/dev/null 2>&1 || true
  docker volume prune -f >/dev/null 2>&1 || true
  find "$DIAG_DIR" -type f -name "*.log" -mtime +1 -delete 2>/dev/null || true
  systemctl reset-failed "$SVC" 2>/dev/null || true

  if job_live; then
    # ISI-3475/ISI-3489: a job is executing. Do NOT wipe go-build (the job's
    # `go test`/`go build` is actively reading it) and do NOT restart (that
    # cancels the job). The docker/diag prune above already reclaimed space;
    # defer the destructive prune + restart to an idle tick.
    logger -t runner-watchdog "disk at ${usep}% >=90; pruned docker+diag, job in progress -> deferred go-build wipe & restart (ISI-3475/3489)"
  else
    rm -rf "${GOCACHE_DIR:?}/"* 2>/dev/null || true
    systemctl restart "$SVC"
    logger -t runner-watchdog "disk at ${usep}% >=90; full prune + restarted (idle)"
  fi
  exit 0
fi

if [ "$(systemctl is-active "$SVC" || true)" = "failed" ]; then
  systemctl reset-failed "$SVC"; systemctl restart "$SVC"
  logger -t runner-watchdog "service was failed; restarted"; exit 0
fi

ts() { local line; line=$(journalctl -u "$SVC" --since "-40 min" --no-pager 2>/dev/null | grep -E "$1" | tail -1 || true)
  [ -z "$line" ] && { echo 0; return; }
  date -d "$(echo "$line" | awk "{print \$1,\$2,\$3}")" +%s 2>/dev/null || echo 0; }
health=$(ts "Listening for Jobs|Running job|Connected to GitHub|Job .* completed")
err=$(ts "Runner connect error|task was canceled|listener exit|No space left")
if [ "$err" -gt "$health" ] && [ $((now - err)) -ge "$STALE_SECS" ]; then
  # A restart here is intentional: the listener is wedged and NOT processing a
  # job, so no in-flight work is lost.
  systemctl reset-failed "$SVC" 2>/dev/null || true; systemctl restart "$SVC"
  logger -t runner-watchdog "stuck retrying >=${STALE_SECS}s; restarted"
fi
