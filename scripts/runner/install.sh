#!/usr/bin/env bash
# Provision the self-hosted runner self-heal + disk-cap guards. ISI-2577/ISI-3475/ISI-3489.
#
# Idempotent: run on each runner host (gitrunner, gitrunner-2, ...) after the
# actions-runner service is installed, and re-run after any re-provision. This
# is the canonical source for /usr/local/sbin/runner-watchdog.sh and
# runner-cache-gc.sh — the scripts used to be deployed out-of-band (ISI-3475
# found a hand-patch on the hosts that a re-provision would have silently
# reverted); keep them here instead.
#
# Usage:  sudo scripts/runner/install.sh
set -euo pipefail

SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SBIN=/usr/local/sbin
UNIT=/etc/systemd/system

if [ "$(id -u)" -ne 0 ]; then
  echo "must run as root (sudo)" >&2; exit 1
fi

install -m 0755 "$SRC/runner-watchdog.sh"  "$SBIN/runner-watchdog.sh"
install -m 0755 "$SRC/runner-cache-gc.sh"  "$SBIN/runner-cache-gc.sh"

for f in runner-watchdog.service runner-watchdog.timer \
         runner-cache-gc.service runner-cache-gc.timer; do
  install -m 0644 "$SRC/systemd/$f" "$UNIT/$f"
done

# Validate syntax before enabling — never ship a broken guard.
bash -n "$SBIN/runner-watchdog.sh"
bash -n "$SBIN/runner-cache-gc.sh"

systemctl daemon-reload
systemctl enable --now runner-watchdog.timer runner-cache-gc.timer

echo "installed. timers:"
systemctl list-timers 'runner-*' --no-pager || true
