# Self-hosted runner self-heal + disk-cap guards

Canonical source for the guards that keep the self-hosted Actions runners
(`gitrunner` @ 10.0.0.190, `gitrunner-2` @ 10.0.0.191) healthy. Lineage:
ISI-2577 (self-heal watchdog) → ISI-3475 (mid-job restart kill) → **ISI-3489**
(persist + cap disk fill).

These scripts were previously **deployed out-of-band** (hand-installed under
`/usr/local/sbin`). ISI-3475 found a hand-patch on both hosts that a
re-provision would have silently reverted. They now live in-repo so provisioning
is reproducible.

## Files

| File | Installed to | Purpose |
|------|--------------|---------|
| `runner-watchdog.sh` | `/usr/local/sbin/` | Reactive self-heal: disk >=90% prune, wedged-listener restart, failed-unit recovery. Restart + go-build wipe are **skipped while a job is live** (ISI-3475/3489). |
| `runner-cache-gc.sh` | `/usr/local/sbin/` | Proactive cap: bounds `GOCACHE` (default 8G) and prunes >24h docker layers so the disk never reaches the reactive 90% threshold. |
| `systemd/runner-watchdog.{service,timer}` | `/etc/systemd/system/` | Runs the watchdog every ~8 min. |
| `systemd/runner-cache-gc.{service,timer}` | `/etc/systemd/system/` | Runs the GC every 20 min. |
| `install.sh` | — | Idempotent installer; run as root on each runner host. |

## Install / re-provision

```bash
sudo scripts/runner/install.sh
```

Host-agnostic: `runner-watchdog.sh` auto-detects the `actions.runner.*` unit, so
the same artifact works on every runner. Tunables via the service files:
`GOCACHE_MAX_GB`, `GOCACHE_DIR`, `STALE_SECS`.

## Why both a reactive and a proactive guard

The watchdog alone is *reactive* — it only acts once disk is already at 90%, and
under CI load `GOCACHE` refilled that fast (~10 min), so it fired ~11x/day and
(pre-ISI-3475) rolling-killed long jobs. `runner-cache-gc` removes the churn at
the source so the watchdog's disk branch is a rare last resort. See
`.github/SELF_HOSTED_RUNNER.md` §7.
