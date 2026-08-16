# Self-hosted GitHub Actions runner — `gitrunner`

The K8squad org exhausted its 2,000 GitHub-hosted Actions minutes, so all CI on
`K8squad/K8squad` moved to a self-hosted runner. This file documents what the
runner must provide and how the workflows target it.

## What the workflows target

Every job uses:

```yaml
runs-on: [self-hosted, linux, x64]
```

A runner is eligible only if it advertises **all three** labels. The default
`actions/runner` install already sets `self-hosted`, `Linux`, `X64`.

## Runner host requirements

Our workflows are Docker-heavy. The runner host (`gitrunner`, 10.0.0.190) must have:

| Requirement | Used by | Notes |
|-------------|---------|-------|
| **Docker Engine** + runner user in `docker` group | `ci.yml` (postgres service), `spine-chaos.yml` (postgres), `e2e.yml` (ollama service + `kind`) | `services:` blocks and `kind` need a working Docker daemon. |
| **Privileged containers** allowed | `build-images.yml` (`docker/setup-qemu-action` binfmt), `e2e.yml` (`kind`) | Multi-arch buildx registers QEMU via a privileged `tonistiigi/binfmt` container. |
| `git`, `curl`, `tar` | `actions/setup-go`, `actions/setup-node` | Toolchains are downloaded into the runner tool cache per run — no pre-baked Go/Node needed, but disk + internet are. |
| **Disk headroom + prune timer** | all | Self-hosted runners do **not** wipe `_work/` or Docker layers between runs. A `docker system prune` systemd timer keeps the disk from filling. |
| Runner installed as a **service** (`svc.sh install`) | all | Survives reboot, auto-restarts, shows `Idle` in org settings. |

## Registration (org admin only)

The runner registers to the **org** `K8squad`, not a single repo, so every repo
can share it. A registration token is short-lived (~1h) and can only be minted by
an **org admin** — a fine-grained PAT without `admin:org` cannot mint one.

```bash
# On an org-admin machine (Henrik):
gh api -X POST /orgs/K8squad/actions/runners/registration-token --jq .token
# …or GitHub UI: Org → Settings → Actions → Runners → New self-hosted runner

# On gitrunner:
cd ~/actions-runner
./config.sh --url https://github.com/K8squad \
            --token <REGISTRATION_TOKEN> \
            --name gitrunner --labels self-hosted,linux,x64 --unattended
sudo ./svc.sh install && sudo ./svc.sh start
```

Then in **Org → Settings → Actions → Runner groups**, allow `K8squad/K8squad`
(and any other repo) to use the runner group.

## Known tradeoff — single runner is serial

One runner executes one job at a time. With matrix builds (`ci` Go matrix,
`build-images`, CodeQL) plus concurrent workflows, jobs will **queue**. Options
as load grows: add more runners with the same labels (they auto-load-balance),
or gate low-value workflows with `concurrency:` groups. Until GitHub-hosted quota
resets or more runners are added, expect serial CI.

## Security note

Self-hosted runners must **not** run untrusted fork-PR code (a fork could run
arbitrary commands on our LAN host). Keep `K8squad/K8squad` private, or in
Org → Settings → Actions set *"Require approval for all outside collaborators"*
for workflows from forks.
