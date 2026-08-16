# Self-Hosted Runner Runbook (`gitrunner`)

**Owner:** DevOps Engineer (config) · ProxOps / Henrik (host access)
**Tracking:** ISI-2602 (migration to self-hosted) · ISI-2612 (runner not staying online)

Every workflow in this repo targets `runs-on: [self-hosted, linux, x64]`. **There are no
GitHub-hosted runners.** If the single self-hosted runner `gitrunner` is not online and draining,
**all** CI/Security/Build/Chaos/DCO jobs sit `queued` with an empty `runner_name` forever — nothing
turns green, and the merge train (ISI-2609) cannot advance.

- **Host:** `gitrunner` @ `10.0.0.190`
- **Registration scope:** repo `K8squad/K8squad` (or org `K8squad` if org-level)
- **Required labels the workflows demand:** `self-hosted`, `linux`, `x64` (all three must be present)

---

## 1. Symptom → this runbook

```
Workflow runs stuck in `queued`, 0 `in_progress`, runner_name = "" (empty),
across CI / Security / Build Images / Spine Chaos / DCO.
```

That is **not** a workflow-label problem (labels on `main` are verified correct — see §5). It means
the runner process on `10.0.0.190` is **not listening**: it either was never installed as a service,
died and did not restart, or is `--ephemeral` and deregistered after one job with no re-registration
loop ("processes one batch then stops").

---

## 2. Diagnose on the host (`10.0.0.190`)

```bash
# Is the runner service installed and running?
systemctl list-units 'actions.runner.*' --all
systemctl status 'actions.runner.*' --no-pager

# Live listener process?
pgrep -af Runner.Listener || echo "NO LISTENER — runner is down"

# Recent runner logs (last job + why it stopped)
sudo journalctl -u 'actions.runner.*' --no-pager -n 120
cd ~/actions-runner && ls -1 _diag/ | tail -3 && tail -n 80 _diag/Runner_*.log

# Is it registered as ephemeral? (ephemeral = deregisters after ONE job)
grep -i ephemeral ~/actions-runner/.runner 2>/dev/null && echo "EPHEMERAL configured"
```

Interpretation:
- **No `actions.runner.*` unit** → runner was started with `./run.sh` in a shell (dies when the
  shell/SSH session closes). Fix = install as a service (§3).
- **Unit exists but `inactive`/`failed`** → crashed and did not restart. Fix = §3 ensures
  `Restart=always`, then `enable --now`.
- **`.runner` shows `"ephemeral": true`** and no supervisor loop → runs exactly one job then exits
  and unregisters. This is the classic "one batch then stops". Fix = re-register **non-ephemeral**
  (§3), or add a re-registration supervisor / adopt ARC (§6).

---

## 3. Fix: install as a persistent auto-restarting service (recommended)

The official runner ships `svc.sh`, which generates a systemd unit with `Restart=always` so the
runner survives job completion, crashes, and reboots.

```bash
cd ~/actions-runner        # the runner install dir

# If a broken/ephemeral registration exists, remove it first:
sudo ./svc.sh stop    2>/dev/null || true
sudo ./svc.sh uninstall 2>/dev/null || true
./config.sh remove --token <REMOVE_TOKEN>     # get token: repo → Settings → Actions → Runners

# Re-register NON-ephemeral with the labels the workflows require:
./config.sh \
  --url https://github.com/K8squad/K8squad \
  --token <REG_TOKEN> \
  --name gitrunner \
  --labels self-hosted,linux,x64 \
  --work _work \
  --replace
#   ^ NOTE: do NOT pass --ephemeral for a single always-on homelab runner.

# Install + enable the service (runs as the given user):
sudo ./svc.sh install $(whoami)
sudo ./svc.sh start
sudo ./svc.sh status          # expect: active (running), Runner.Listener alive

# Belt-and-suspenders: survive host reboot
sudo systemctl enable 'actions.runner.K8squad-K8squad.gitrunner.service'
```

`<REG_TOKEN>` / `<REMOVE_TOKEN>` are short-lived tokens from **repo → Settings → Actions → Runners →
New self-hosted runner** (or `Remove`). They are *not* the `GH_token` PAT.

---

## 4. Verify the fix (from any host with the PAT)

```bash
# The runner should appear "online" and idle, then pick up queued jobs within seconds.
gh api repos/K8squad/K8squad/actions/runners           # needs a PAT with manage_runners
# or watch the queue drain:
gh run list --repo K8squad/K8squad --limit 8
```

Success = previously `queued` runs flip to `in_progress` with `runner_name=gitrunner`, then
`completed`. Confirm on the host with `pgrep -af Runner.Worker` during a job.

> The board's `GH_token` PAT currently returns **403** on the runner-admin endpoints
> (`actions/runners`) — it lacks `manage_runners`/admin scope. If runner status must be inspected via
> API, mint a PAT with `repo` + `manage_runners` (repo admin), or read status in the repo Settings UI.

---

## 5. Workflow-label state (verified — NOT the cause)

As of `main@f37c5bf` every job in every workflow already targets the correct labels
(migration ISI-2602 landed). No workflow edit is needed:

| Workflow | `runs-on` |
|----------|-----------|
| `ci.yml` (3 jobs) | `[self-hosted, linux, x64]` |
| `security.yml` (5 jobs) | `[self-hosted, linux, x64]` |
| `build-images.yml` | `[self-hosted, linux, x64]` |
| `spine-chaos.yml` | `[self-hosted, linux, x64]` |
| `e2e.yml` (2 jobs) | `[self-hosted, linux, x64]` |
| `dco.yml` | `[self-hosted, linux, x64]` |

**Caveat for open PRs:** any PR branch cut *before* the ISI-2602 migration still carries
`runs-on: ubuntu-latest` and its jobs die unassigned (there is no ubuntu runner). Fix = **rebase the
PR on current `main`**; that both adopts self-hosted `runs-on` and picks up the PR's own toolchain
fix. (Merge train: ISI-2609.)

---

## 6. Hardening / follow-ups (post-unblock)

- **Availability:** a single runner is a single point of failure and serializes the whole merge
  train. Add a **second** self-hosted runner (same labels) so jobs parallelize, or adopt
  **Actions Runner Controller (ARC)** on the cluster for ephemeral, auto-scaled, per-job pods
  (clean isolation + no "one batch then stops" class of bug).
- **If ephemeral is a security requirement:** ephemeral runners *must* be paired with a
  re-registration mechanism (ARC, or a `while true; do ./config.sh … ; ./run.sh --once; done`
  supervisor with a JIT token) — a bare `--ephemeral` runner will always stall after one job.
- **Watchdog:** a lightweight systemd timer or Prometheus blackbox check that alerts when
  `Runner.Listener` is absent for > N minutes closes the "silent death" gap that caused ISI-2612.
- **Resource guard:** ensure the host has disk headroom for `_work/`; a full `_work` disk also
  presents as jobs that never start.
