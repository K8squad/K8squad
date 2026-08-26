# Getting Started: the BMAD squad

Go from an empty Kubernetes cluster to a running **BMAD** squad — 13 pre-defined
roles in a CEO → PM/Architect/UX hierarchy, wired with four default Skills — in
about ten minutes.

**TL;DR**

```bash
# 1. Install the operator (once per cluster)
helm repo add ksquad https://charts.k8squad.io
helm install ksquad ksquad/k8squad --namespace k8squad-system --create-namespace

# 2. Apply the predefined BMAD squad
kubectl apply -f examples/bmad-team/

# 3. Put a real model token in the credentials Secret
kubectl -n bmad edit secret model-credentials    # replace REPLACE_ME

# 4. Watch the team come up
kubectl -n bmad get team,agents,roles,skills
```

That is the whole journey. The rest of this guide explains each step, what you
just deployed, and how to kick off a first Run.

> **Heads-up on filenames.** The manifests live in
> [`examples/bmad-team/`](../examples/bmad-team/) and are built in ISI-3270. This
> guide mirrors that folder's planned layout and apply-order; if a filename here
> ever disagrees with the folder, the folder wins — treat its own `README.md` as
> the source of truth and [open an issue](https://github.com/K8squad/K8squad/issues)
> so we can reconcile.

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
`Role`, `Skill`, `Project`, `Run`) and reconciles them. Install it once:

```bash
helm repo add ksquad https://charts.k8squad.io
helm repo update
helm install ksquad ksquad/k8squad \
  --namespace k8squad-system --create-namespace

# Wait for the control plane to be Ready
kubectl -n k8squad-system rollout status deploy/ksquad-operator
```

If you would rather build from source, see
[CONTRIBUTING.md](../CONTRIBUTING.md).

---

## Step 1 — Apply the BMAD squad

The predefined squad is a self-contained, `kubectl apply`-able bundle. Every
cross-resource reference resolves inside the folder, so a single apply leaves
nothing dangling:

```bash
kubectl apply -f examples/bmad-team/
```

`kubectl` applies every manifest in the directory; the operator resolves the
namespace, Secret, prompt ConfigMaps, Skills, runtimes, Roles, Agents, Project
and Team regardless of file order. If you prefer one file, apply the
concatenated bundle instead:

```bash
kubectl apply -f examples/bmad-team/squad.yaml
```

Everything lands in a dedicated **`bmad`** namespace so it never collides with
the `k8squad-demo` quickstart squad.

---

## Step 2 — Set the model credentials

The squad ships with a placeholder token so `apply` never fails, but the agents
cannot authenticate until you swap it for a real one. The token lives in a
`Secret` named `model-credentials`:

```bash
kubectl -n bmad edit secret model-credentials
# find stringData.token: "REPLACE_ME" and paste your real token
```

Or set it non-interactively:

```bash
kubectl -n bmad create secret generic model-credentials \
  --from-literal=token='sk-your-real-token' \
  --dry-run=client -o yaml | kubectl apply -f -
```

Every Agent references this one Secret by name and key
(`credentialSecretRef: { name: model-credentials, key: token }`), so you set the
credential in exactly one place.

---

## Step 3 — Meet the team

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

`Skill` is a first-class CR. The squad grants four defaults and attaches them to
the roles that need them via each Role's `defaultSkills[]` (an agent inherits its
role's defaults unless it overrides them with its own `skillRefs[]`):

| Skill | Purpose | Attached to |
|-------|---------|-------------|
| **`bmad`** | The BMAD method/process skill — the shared way of working. | Every role |
| **`github`** | Source control: branches, PRs, reviews. | Coder, DevOps Engineer, Code Reviewer |
| **`dynatrace`** (`dtctl`) | Observability queries and dashboards. | Observability Engineer |
| **`graphical`** | Visual/diagram generation. | Graphical Designer |

Each Skill declares its own `source` (`inline` or `git`), `permissions` (the
authorized capability envelope), and `requires` (toolchains and sidecars the
operator provisions). Research is ranking additional dev/debugging skills
(ISI-3271); those fold in later without changing these four.

---

## Step 4 — Verify the team is live

```bash
# All resources present
kubectl -n bmad get team,agents,roles,skills,project

# The Team should report its agents composed and Ready
kubectl -n bmad get team bmad-squad -o wide
kubectl -n bmad describe team bmad-squad

# Spot-check that an Agent resolved its role, runtime, skills and credential
kubectl -n bmad describe agent ceo
```

You want the `Team` status to show its agents reconciled and no events
complaining about a missing `*Ref`. If an Agent is stuck, `describe` it — the
most common causes are the credentials Secret still holding `REPLACE_ME`, or a
prompt ConfigMap that failed to apply.

---

## Step 5 — Kick off your first Run

A `Run` is a unit of orchestrated work against the team's `Project`. Point the
squad at a task and let the CEO fan it out through the hierarchy.

Open the console to drive it from the UI:

```bash
kubectl port-forward -n k8squad-system svc/ksquad-console 8080:80
# → http://localhost:8080  — pick the bmad team, create a Run, watch it stream
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
- **4 `Skill`s** — `bmad`, `github`, `dynatrace`, `graphical`.
- **1+ `AgentRuntime`** — the coding-agent flavor (e.g. `claude-code`).
- **1 `Project`** — the repository the squad works on.
- **1 `Secret`** — your model token, referenced everywhere.

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---------|-------------|-----|
| Agents never become Ready | Secret still holds `REPLACE_ME` | Set a real token (Step 2). |
| `apply` errors on unknown kind `ksquad.io/...` | Operator/CRDs not installed | Run the Helm install (Prerequisites). |
| An Agent event says a `*Ref` is missing | Applied a subset of the folder | Re-apply the whole `examples/bmad-team/`. |
| Console shows no team | Wrong namespace | The squad lives in `bmad`, not `k8squad-demo`. |

---

## Next steps

- Read the [architecture overview](../README.md#-architecture).
- Explore the [quickstart squad](../hack/quickstart/squad.yaml) — the minimal
  one-agent version this squad extends.
- Full CRD and API reference: <https://k8squad.io/docs>.

Clean up when you are done:

```bash
kubectl delete -f examples/bmad-team/
```
