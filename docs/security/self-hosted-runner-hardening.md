# Self-Hosted CI Runner Hardening

**Ticket:** ISI-3229 (follow-up from ISI-3223 security review)
**Owner:** DevOps
**Status:** partial — workflow-layer controls landed; runner-VM + admin + network items tracked below.

## Threat model

`K8squad/K8squad` is a **public** repository whose CI runs on a **self-hosted**
runner fleet (`gitrunner-1/2/3`, label `[self-hosted, linux, x64]`). The repo has
no GitHub-hosted minutes, so every job — including fork-triggered `pull_request`
jobs — lands on our own VMs.

The core risk is **arbitrary code execution on the runner**: a fork PR can modify
build steps, test code, or Makefile targets and have them execute on our
infrastructure. Secondary risks are **secret exfiltration** (if a job holds a
write token or repo secrets) and **lateral movement** from the runner VLAN into
the management / CAPMOX plane.

## Controls

### ✅ Landed in this PR (workflow layer)

| # | Control | Where |
|---|---------|-------|
| 2a | **All third-party actions SHA-pinned** (84 `uses:` across 11 workflows) to full commit SHAs with `# vX.Y.Z` comments — kills mutable-tag supply-chain takeover. | `.github/workflows/*.yml` |
| 2b | **Dependabot** for `github-actions` (+ `gomod`) keeps SHA pins fresh — bumps SHA *and* comment on new releases. | `.github/dependabot.yml` |
| 3 | **Least-privilege `permissions:`** confirmed on all 13 workflows: read-only default (`contents: read`) with per-workflow escalation only where required (`build-images` → `packages/id-token/attestations: write`; `helm-release` → `contents: write` for gh-pages; `security` → `security-events: write` for CodeQL SARIF). No workflow grants blanket write. | audited — no change needed |
| 4 | **`pull_request_target` red-line enforced as CI.** `workflow-policy.yml` fails the build if any workflow introduces `pull_request_target`, lacks a top-level `permissions:` block, or uses an unpinned third-party action. | `.github/workflows/workflow-policy.yml` |

`pull_request_target` was already absent from the codebase; the guard prevents
re-introduction rather than removing an existing use.

### ⏳ Runner-VM work — DevOps, blocked on registration tokens (item 1)

Convert `gitrunner-1/2/3` (+ watchdog) to **`--ephemeral`** registration so each
runner processes exactly one job, then de-registers and re-provisions on a clean
workspace. This defeats cross-job workspace poisoning (a fork PR cannot leave
artifacts behind for the next job) and is the single most important
runner-side control.

**Blocker:** ephemeral mode is set at `config.sh` registration time and requires
a fresh **runner registration token**, which the current DevOps PAT cannot mint
(fine-grained token lacks the self-hosted-runner admin scope — see ISI-2577 /
`gitrunner-selfhosted-runner`). Henrik (or a PAT with `administration:write` on
the repo) must supply registration tokens, after which the systemd unit's
`ExecStart` re-runs `config.sh --ephemeral --replace` and the watchdog re-registers
per job. **Unblock owner: Henrik.**

### 🔒 Admin-only — Henrik (tracked in ISI-3223)

- Set **"Require approval for all external contributors"** on Actions so fork PRs
  never auto-run on self-hosted runners without a maintainer's click.
- Enable **`main` branch protection** (required reviews + required status checks).

### 🌐 Network isolation — DevOps + Henrik (item 5)

Isolate the runner VLAN from the `10.0.0.0/24` management / CAPMOX plane
(runners currently share the LAN with Proxmox + the CAPI management cluster).
A compromised runner should not reach the hypervisor API or mgmt-cluster kube-API.
Needs a firewall/VLAN change on the homelab network — **coordinate with Henrik.**

## Verification

`workflow-policy.yml` runs on every PR touching `.github/workflows/**` and on
push to `main`, providing continuous enforcement of controls 2a, 3, and 4.
