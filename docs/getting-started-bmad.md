# Getting Started: the BMAD squad

Go from an empty Kubernetes cluster to a running **BMAD** squad — 13 pre-defined
roles in a CEO → PM/Architect/UX hierarchy, wired with four default Skills — in
about ten minutes.

**TL;DR**

```bash
# 1. Install the operator + the default toolchain catalog (once per cluster)
helm repo add ksquad https://charts.k8squad.io
helm install ksquad ksquad/k8squad --namespace k8squad-system --create-namespace \
  --set tools.defaultCatalog.enabled=true

# 2. Apply the predefined BMAD squad — Team, Roles, Agents, Project, Skills.
#    The tool Skills reach GitHub/Dynatrace/rendering through CLI toolchains,
#    so there are no MCP servers to set up.
kubectl apply -f examples/bmad-team/squad.yaml

# 3. Put a real token in the model-credentials Secret
kubectl -n bmad-squad edit secret model-credentials

# 4. Watch the team come up
kubectl -n bmad-squad get team,agents,roles,skills

# 5. (Optional) add the dev/debug Skills from the canonical catalog
kubectl apply -k github.com/K8squad/k8squad-skills

# 6. (Optional, advanced) only if you add a skill whose tools are reachable
#    ONLY over MCP: apply the MCPServer reference example + its token Secrets
# kubectl apply -f examples/bmad-team/02b-mcpservers.yaml
```

That is the whole journey. The rest of this guide explains each step, what you
just deployed, and how to kick off a first Run.

> **Heads-up on sources.** Two repos back this squad:
> - The **team** manifests live in this repo under
>   [`examples/bmad-team/`](../examples/bmad-team/) (ISI-3270) — Team, Roles,
>   Agents, Project, the credentials Secrets and **the 4 default Skills**
>   (`03-skills.yaml`), all in the `bmad-squad` namespace. One apply of
>   `squad.yaml` gives you a working squad. (`02b-mcpservers.yaml` is an
>   optional MCPServer reference example the default skills don't use.)
> - The dedicated [`K8squad/k8squad-skills`](https://github.com/K8squad/k8squad-skills)
>   repo (ISI-3274) is the **canonical Skill catalog** — the authoritative
>   definitions of those 4 defaults *plus* the dev/debug skill set. You only
>   apply it when you want the extra dev/debug capabilities (Step 2).
>
> If a filename or apply path here ever disagrees with the repo it comes from,
> the repo wins — treat its own `README.md` as the source of truth and
> [open an issue](https://github.com/K8squad/K8squad/issues) so we can reconcile.

---

## Prerequisites

You need three things before you start:

1. **A Kubernetes cluster, 1.31 or newer.** A real cluster (managed or
   self-hosted) — the BMAD squad schedules sandboxed workloads, so a laptop
   `kind`/`minikube` cluster works for a smoke test but a multi-node cluster is
   what you want for real work.
2. **`kubectl` and `helm`**, pointed at that cluster (`kubectl cluster-info`
   should succeed).
3. **A model API token** — an Anthropic/Claude key (or whichever model provider
   your `AgentRuntime` targets). You paste this into a Secret in step 3.

The K8squad **operator** installs the CRDs (`Team`, `Agent`, `AgentRuntime`,
`Role`, `Skill`, `Project`, `Run`, `MCPServer`, `Toolchain`) and reconciles
them. Install it once — and switch on the **default toolchain catalog**, which
the squad's tool Skills resolve against:

```bash
helm repo add ksquad https://charts.k8squad.io
helm repo update
helm install ksquad ksquad/k8squad \
  --namespace k8squad-system --create-namespace \
  --set tools.defaultCatalog.enabled=true

# Wait for the control plane to be Ready
kubectl -n k8squad-system rollout status deploy/ksquad-operator
```

That one flag renders the curated catalog — `kubectl`, `git`, `gh`, `go`,
`node`, `dtctl`, `helm`, `python`, `docker-cli`, `uv`, `jq`, `yq`, `curl`,
`make` — as `Toolchain` objects in `k8squad-system`, each a
pinned image plus a least-privilege RBAC declaration (kubectl gets read-only
core+apps; the rest get staged onto `PATH` with no Kubernetes API grant at
all). Verify it landed:

```bash
kubectl get toolchains -n k8squad-system
```

Without the catalog, Runs whose Skills require `gh@2.98`, `dtctl@1.0` or
`node@22` fail admission with an actionable message naming the demanding
skill — nothing stages silently.

If you would rather build from source, see
[CONTRIBUTING.md](../CONTRIBUTING.md).

---

## The capability-plane checklist

The squad's tool Skills resolve against more than the folder you apply. Two
requirements for the default squad, plus an optional advanced one, decide
whether your first Run admits:

| # | Requirement | Where |
|---|-------------|-------|
| 1 | **Toolchain catalog enabled** — the Skills' `requires.toolchains` (`gh@2.98`, `dtctl@1.0`, `node@22`) resolve `name@version` against `Toolchain` objects. Install the operator with `--set tools.defaultCatalog.enabled=true` or Runs fail admission naming the missing toolchain. | [Prerequisites](#prerequisites) |
| 2 | **Skill sources SHA-pinned** — git-sourced Skills must carry an immutable commit `ref`, not a floating branch. The bundled SHAs point at real catalog commits; repoint them if you fork the catalog. | [Step 4](#sha-pinned-skill-sources) |
| — | **(Optional, advanced) MCP servers** — only if you add a skill whose tools are reachable *only* over MCP: apply `02b-mcpservers.yaml`, give the skill an `mcpToolRefs`, set the token Secrets (not `REPLACE_ME`), and wait one discovery probe cycle (`initialize` → `tools/list`) before Runs referencing it admit. The default skills use CLI toolchains and need none of this. | [Step 3](#step-3--set-the-credentials-model--optional-mcp-servers) |

---

## Step 1 — Apply the BMAD squad

The predefined team is a `kubectl apply`-able bundle. Apply the concatenated
single-file bundle:

```bash
kubectl apply -f examples/bmad-team/squad.yaml
```

`squad.yaml` carries the namespace, Secret, prompt ConfigMaps, runtimes,
Skills, Roles, Agents, Project and Team; the operator resolves them regardless
of order. (You can also apply the numbered files individually — see the folder
README — but skip the optional `02b-mcpservers.yaml`, which `squad.yaml`
deliberately leaves out.)

Everything — including the four default `Skill` CRs (`03-skills.yaml`) — lands
in a dedicated **`bmad-squad`** namespace so it never collides with the
`k8squad-demo` quickstart squad. The tool Skills reach their tools through CLI
toolchains, so no `MCPServer` is applied by default (`02b-mcpservers.yaml` is
an optional reference example). The Roles reference those Skills by name via
their `defaultSkills[]`, and they apply in the same bundle, so the squad is
self-contained: this one apply is enough to bring the team up.

---

## Step 2 — (Optional) add the dev/debug Skills

The four defaults (`bmad`, `github`, `dynatrace`, `graphical`) already applied
with the squad in Step 1, so you can skip straight to credentials. This step is
for when you want more.

The [`K8squad/k8squad-skills`](https://github.com/K8squad/k8squad-skills) repo is
the **canonical Skill catalog** — the source of truth for the 4 defaults *plus*
a dev/debug set (`code-search`, `kubectl-debug`, `go-build-test`, `git-workflow`,
`golangci-lint`, `otel-observability-query`, `container-build`, `delve-pprof`,
`psql-inspect`, and the optional `http-grpc-probe`). Apply the whole catalog
straight from the repo with Kustomize — its `kustomization.yaml` stamps the
`bmad-squad` namespace, so no clone or `-n` override is needed:

```bash
kubectl apply -k github.com/K8squad/k8squad-skills

# …or a single skill:
kubectl apply -f https://raw.githubusercontent.com/K8squad/k8squad-skills/main/skills/kubectl-debug/skill.yaml
```

(Prefer a local clone? `git clone https://github.com/K8squad/k8squad-skills.git
&& kubectl apply -k k8squad-skills/` does the same.)

Re-applying is safe — the catalog is the authoritative definition of the same 4
defaults, so this simply layers the dev/debug skills alongside them. Then wire a
skill to the roles or agents that should have it:

```yaml
# grant to every agent assuming a role
kind: Role
spec:
  defaultSkills:
    - name: code-search
    - name: go-build-test
---
# grant to one specific agent (overrides the role defaults)
kind: Agent
spec:
  skillRefs:
    - name: delve-pprof
```

See the repo's own `README.md` for the per-skill purpose, the least-privilege
`permissions[]`, and the recommended role→skill matrix.

---

## Step 3 — Set the credentials (model + optional MCP servers)

The squad ships with placeholder tokens so `apply` never fails, but the agents
cannot authenticate until you swap them for real ones.

**Model token** — lives in a `Secret` named `model-credentials`:

```bash
kubectl -n bmad-squad edit secret model-credentials
# find stringData.token: "REPLACE_ME" and paste your real token
```

Or set it non-interactively:

```bash
kubectl -n bmad-squad create secret generic model-credentials \
  --from-literal=token='sk-your-real-token' \
  --dry-run=client -o yaml | kubectl apply -f -
```

Every Agent references this one Secret by name and key
(`credentialSecretRef: { name: model-credentials, key: token }`), so you set the
credential in exactly one place.

**Tool tokens (CLI toolchains)** — the `github` and `dynatrace` skills drive
the `gh` and `dtctl` CLIs, which authenticate from an env-var token (e.g.
`GH_TOKEN`, `DT_API_TOKEN`) projected into the Run pod — never a file the
runtime reads (ADR-045 D5). Supply those via the agent's credential
projection (see the per-skill READMEs in
[`k8squad-skills`](https://github.com/K8squad/k8squad-skills)); a missing
token just leaves the CLI unauthenticated, it does not block admission.

**(Optional, advanced) MCP server tokens** — *only* if you opted into
`02b-mcpservers.yaml` for an MCP-backed skill. The `github-mcp` /
`dynatrace-mcp` servers there each reference their own Secret
(`github-mcp-token`, `dynatrace-mcp-token`), because credentials never ride
inside the CRD itself:

```bash
kubectl -n bmad-squad create secret generic github-mcp-token \
  --from-literal=token='ghp_your_github_token' \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n bmad-squad create secret generic dynatrace-mcp-token \
  --from-literal=token='dt0c01-your-dynatrace-token' \
  --dry-run=client -o yaml | kubectl apply -f -
```

The control-plane discovery controller then probes each server (initialize →
`tools/list`) and records the tool surface on `status.observedTools`. A Run
referencing an MCP server fails closed until discovery has succeeded — check
the conditions and give it one probe cycle:

```bash
kubectl -n bmad-squad get mcpservers -o wide
# NAME             TRANSPORT         READY   TOOLS   AGE
# github-mcp       streamable-http   True    14      2m
# dynatrace-mcp    stdio             True    9       2m
```

---

## Step 4 — Meet the team

The squad encodes the classic **BMAD** flow as 13 roles. There is no
`reportsTo` field on the CRDs — the reporting hierarchy lives in each Role's
prompt (and in labels), and the `Team` itself is a flat list of agents. The
hierarchy the prompts describe is:

```
CEO
├── Product Manager
│   ├── Brainstormer
│   ├── Challenger
│   └── Content Writer
├── Architect
│   ├── Code Reviewer
│   ├── Test Architect
│   ├── Coder
│   ├── DevOps Engineer
│   └── Observability Engineer
└── UX Designer
    └── Graphical Designer
```

| Role | Reports to | What it does |
|------|-----------|--------------|
| **CEO** | — | Sets direction, arbitrates, owns the outcome. |
| **Product Manager** | CEO | Turns intent into scoped, prioritized work. |
| **Architect** | CEO | Owns the technical design and build sequencing. |
| **UX Designer** | CEO | Owns the user experience and flows. |
| **Brainstormer** | Product Manager | Generates ideas and options. |
| **Challenger** | Product Manager | Adversarially stress-tests proposals. |
| **Content Writer** | Product Manager | Produces docs, guides, and announcements. |
| **Code Reviewer** | Architect | Reviews changes for correctness and quality. |
| **Test Architect** | Architect | Designs the test strategy and coverage. |
| **Coder** | Architect | Implements the work. |
| **DevOps Engineer** | Architect | Owns CI/CD, infra, and release. |
| **Observability Engineer** | Architect | Owns telemetry, dashboards, and SLOs. |
| **Graphical Designer** | UX Designer | Produces visual assets. |

Each role is one `Role` CR (with a prompt `ConfigMap`) plus one `Agent` CR, all
composed flat inside a single `Team`.

### The four default Skills

`Skill` is a first-class CR. The squad ships four defaults in its bundle
(`03-skills.yaml`; canonical definitions in the
[`K8squad/k8squad-skills`](https://github.com/K8squad/k8squad-skills) catalog)
and attaches them to the roles that need them via each Role's `defaultSkills[]`
(an agent inherits its role's defaults unless it overrides them with its own
`skillRefs[]`):

| Skill | Purpose | Attached to |
|-------|---------|-------------|
| **`bmad`** | The BMAD method/process skill — the shared way of working. | Every role |
| **`github`** | Source control: branches, PRs, reviews. | Coder, DevOps Engineer, Code Reviewer |
| **`dynatrace`** (`dtctl`) | Observability queries and dashboards. | Observability Engineer |
| **`graphical`** | Visual/diagram generation. | Graphical Designer |

Each Skill declares its own `source` (`inline` or `git`), `permissions` (the
authorized capability envelope), and `requires` (toolchains and sidecars the
operator provisions). The `requires.toolchains` refs — `gh@2.98`,
`dtctl@1.0`, `node@22` — resolve `name@version` against the catalog you
enabled at install time; these tools are plain CLIs, so the `github` /
`dynatrace` Skills reach them through their toolchains and carry **no**
`mcpToolRefs` (MCP is the optional advanced path in `02b-mcpservers.yaml`,
for tools reachable only over MCP). Beyond these four, the catalog carries a
**dev/debug set** (code-search, kubectl-debug, go-build-test, and more — see
Step 2) you can layer on per role or per agent.

### SHA-pinned skill sources

The three tool Skills are **git-sourced**, and their `source.git.ref` is
pinned to an immutable **commit SHA** — never a floating branch:

```yaml
source:
  git:
    repoRef: github.com/K8squad/k8squad-skills
    ref: "bf3bc86b338d488fe751c289943db194ce8102c7"   # 40-hex commit SHA
    path: skills/github
```

This is deliberate: a `ref` that floats (a branch name) lets a force-push
silently change what a skill does between one Run and the next, or worse,
mid-Run. A pinned SHA means the exact bytes the operator stages are the exact
bytes you reviewed. It is the same reproducibility discipline as the
digest-pinned toolchain images.

The SHAs shipped in `03-skills.yaml` point at **real catalog commits**
(`bf3bc86` = `k8squad-skills` main). Repoint each `ref` if you fork the
catalog — at:

- the published SHA of the corresponding skill revision in the
  [`K8squad/k8squad-skills`](https://github.com/K8squad/k8squad-skills)
  catalog (the catalog's README carries the current pinned SHAs), **or**
- a commit SHA from your own skill repository.

Bump the SHA deliberately — pin, review, advance — the same way you would
bump a dependency version.

---

## Step 5 — Verify the team is live

```bash
# All resources present
kubectl -n bmad-squad get team,agents,roles,skills,project

# The toolchain catalog the squad resolves against
kubectl get toolchains -n k8squad-system

# The Team should report its agents composed and Ready
kubectl -n bmad-squad get team bmad-squad -o wide
kubectl -n bmad-squad describe team bmad-squad

# Spot-check that an Agent resolved its role, runtime, skills and credential
kubectl -n bmad-squad describe agent ceo
```

You want the `Team` status to show its agents reconciled and no events
complaining about a missing `*Ref`. If an Agent is stuck, `describe` it — the
most common causes are the `model-credentials` Secret still holding
`REPLACE_ME`, or a prompt ConfigMap or default `Skill` from the bundle that
failed to apply (re-apply `examples/bmad-team/squad.yaml`). (If you opted into
the optional `02b-mcpservers.yaml`, also check each `MCPServer` is
`Ready=True` with a non-empty `TOOLS` count.)

### What a Run actually assembles

When you kick off a Run, the operator resolves the participating agents'
Skills, unions their requirements, and builds the sandbox pod around them:

- every resolved toolchain (`gh@2.98`, `dtctl@1.0`, `node@22`) is staged by an
  **init container** onto a shared volume, mounted read-only, with `PATH`
  pointing at it — `gh` and `dtctl` are on `PATH` before the agent runtime
  starts;
- (only if a skill references an `MCPServer`) a stdio MCP server runs as a
  **sidecar** and a streamable-http one is wired into the runtime's native MCP
  config, scoped to the tools its `toolFilter` allows — the default skills use
  no MCP server, so this step is a no-op for them;
- the unioned toolchain RBAC is rendered as **one per-Run `Role`** bound to the
  squad ServiceAccount — gone when the Run completes;
- the resolved set (images, tool filters, granted RBAC) is recorded as the
  **capability manifest** on `Run.status`, so a running Run never widens
  mid-flight and the audit trail survives after the fact.

Version conflicts across a Run's skills (say one wants `node@22`, another
`node@20`) fail Run admission up front, not at container start.

---

## Step 6 — Kick off your first Run

A `Run` is a unit of orchestrated work against the team's `Project`. Point the
squad at a task and let the CEO fan it out through the hierarchy.

Open the console to drive it from the UI:

```bash
kubectl port-forward -n k8squad-system svc/ksquad-console 8080:80
# → http://localhost:8080  — pick the bmad-squad team, create a Run, watch it stream
```

From the console you create a Run, give it a goal, and watch progress stream
live over SSE as the PM scopes it, the Architect sequences it, and the Coder /
reviewers execute. The same is possible declaratively with a `Run` CR — see the
[CRD reference](https://k8squad.io/docs) for its shape.

---

## What you just deployed

- **1 `Team`** (`bmad-squad`) — the tenancy boundary and squad composition.
- **13 `Role`s + 13 prompt `ConfigMap`s** — behavior and the reporting hierarchy.
- **13 `Agent`s** — one per role, each bound to a model, runtime, role, and the
  shared credential.
- **4 default `Skill`s** — `bmad`, `github`, `dynatrace`, `graphical` (in the
  bundle's `03-skills.yaml`; canonical definitions live in
  [`K8squad/k8squad-skills`](https://github.com/K8squad/k8squad-skills), which
  also carries the optional dev/debug set).
- **1 `Secret`** — your `model-credentials` model token, referenced by every
  Agent. (The tool CLIs authenticate from env-var tokens projected per Run;
  see Step 3.)
- **1+ `AgentRuntime`** — the coding-agent flavor (e.g. `claude-code`).
- **1 `Project`** — the repository the squad works on.

No `MCPServer` is applied by default — the tool Skills use CLI toolchains.
`02b-mcpservers.yaml` (two illustrative `MCPServer`s + their token Secrets)
stays on the shelf until you add an MCP-backed skill.

Plus, at the cluster level: the **`Toolchain` catalog** in `k8squad-system`
(the curated tool set, enabled with `tools.defaultCatalog.enabled=true`) that
the squad's `requires.toolchains` refs resolve against.

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---------|-------------|-----|
| Agents never become Ready | Secret still holds `REPLACE_ME` | Set a real token (Step 3). |
| An Agent references a dev/debug skill that doesn't exist | Catalog not applied | `kubectl apply -k github.com/K8squad/k8squad-skills` (Step 2). |
| Run admission rejects with `toolchain ... not resolved` | Default catalog disabled | Reinstall/upgrade with `--set tools.defaultCatalog.enabled=true` (Prerequisites), or apply your own `Toolchain` objects. |
| `MCPServer` stuck `Ready=False`, `ToolsDiscovered=False` | Discovery has not succeeded yet — bad endpoint, missing token, or first probe still pending | Check `kubectl -n bmad-squad describe mcpserver <name>`; verify the endpoint and the token Secret (Step 3), then wait one probe cycle. |
| Run rejects an MCP-referencing skill | Server tool surface unknown (fail-closed staleness) | Same as above — Runs admit only against discovered tools. |
| `apply` errors on unknown kind `ksquad.io/...` | Operator/CRDs not installed | Run the Helm install (Prerequisites). |
| An Agent event says a `*Ref` is missing | Applied a subset of the folder | Re-apply the whole `examples/bmad-team/`. |
| Console shows no team | Wrong namespace | The squad lives in `bmad-squad`, not `k8squad-demo`. |

---

## Next steps

- Read the [architecture overview](../README.md#-architecture).
- Explore the [quickstart squad](../hack/quickstart/squad.yaml) — the minimal
  one-agent version this squad extends.
- **Add your own tool** — when a skill needs a CLI the default catalog does not
  ship, follow [Adding a toolchain](https://github.com/K8squad/k8squad-skills/blob/main/docs/adding-a-toolchain.md):
  build the tool image, wire it into the catalog (or your team namespace), and
  pin `name@version` so `requires.toolchains` resolves.
- Full CRD and API reference: <https://k8squad.io/docs>.

Clean up when you are done:

```bash
kubectl delete -f examples/bmad-team/
```
