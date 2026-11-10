<div align="center">

<img src="docs/media/banner-on-dark.png" alt="K8squad — Your agents, in formation" width="720">

### Autonomous agent squads on Kubernetes — your agents, in formation.

[![CI](https://github.com/K8squad/K8squad/actions/workflows/ci.yml/badge.svg)](https://github.com/K8squad/K8squad/actions/workflows/ci.yml)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](./LICENSE)
[![Go 1.23](https://img.shields.io/badge/Go-1.23-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Kubernetes 1.31](https://img.shields.io/badge/Kubernetes-1.31-326CE5?logo=kubernetes&logoColor=white)](https://kubernetes.io/)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](./CONTRIBUTING.md)

[Website](https://k8squad.io) · [Documentation](https://k8squad.io/docs) · [Quickstart](#-quickstart) · [Architecture](#-architecture) · [Contributing](./CONTRIBUTING.md)

</div>

---

**KSquad** organizes AI agents into **squads** — virtual teams that coordinate on shared
work items instead of ad-hoc peer-to-peer chat. It is an operator-based platform: teams, agents,
roles, skills, projects, and runs are all first-class Kubernetes custom resources, reconciled by
controllers. Bring your own agent runtime, bring your own model, and let the cluster do the
formation-keeping.

> **Status:** early development. CRDs and interfaces are `v1alpha1` and not yet stable.

## ✨ Features

- **🤖 Autonomous agent squads** — mix heterogeneous runtimes (**Claude Code, Codex, OpenCode, OpenClaw, Hermes, Vibe, Goose, CrewAI**) in one squad. All eight are conformant runtime types; vendor shims ride `spec.experimental=true`. See [supported runtimes](./docs/supported-agents.md).
- **🕸️ AgentMesh** — runtimes plug into a mesh with three seams (runtime adapter, platform host, scheduling policy), so new agent flavors land with **zero control-plane change**. Warm sandboxes hibernate when idle and **rebind-at-wake** if the model moved; CrewAI crews run as always-on pools. See [AgentMesh architecture](./docs/agent-mesh.md).
- **📋 Agents that work the board** — agents hold custody and move their own work through `todo` → `in_progress` → `in_review` ([ADR-0025](./docs/adr/0025-agent-status-change.md)), raise **decision requests** with ≥2 ranked options (DecisionCard inbox, one-key answers), propose next steps, and leave one clean run summary instead of log noise. Humans stay the completion authority. See [board-working agents](./docs/board-working-agents.md).
- **🔁 Resilient dispatch** — **mid-run model fallback**: on a rate-limit verdict the same run continues on a configured fallback model, or pauses cleanly if none is set. See [mid-run fallback](./docs/mid-run-model-fallback.md).
- **🌡️ Warm pools by policy** — `RolePool` target-N prescheduling is policy #1 of the mesh scheduling seam: coordinator floors, always-up exemptions, saturation ceilings. See [scheduling policy](./docs/scheduling-policy.md).
- **🗂️ Project-scoped work management** — a Kanban-style work-item tree with tickets, sub-tickets, checkout/claim/lease, comments, and artifacts — an append-only, provenanced coordination record, with GitHub-synced Kanban views and live run markers.
- **🔒 RBAC, isolation & a credentials vault** — every `Team` is a tenancy boundary with its own namespace, NetworkPolicy, and quota. Agent code runs untrusted in gVisor-sandboxed pods. BYO credentials live in the **Credentials Vault** (save / test / rotate / delete, with audit telemetry).
- **📡 OTel-native observability** — traces, metrics, and logs are OpenTelemetry-first; W3C trace context propagates dispatch → gateway → mesh child, and `ksquad.ticket.ref` joins every span back to the ticket. See [distributed tracing](./docs/observability/distributed-tracing.md).
- **🧩 Plugin SDK** *(planned)* — an extension model where plugins subscribe to a NATS/JetStream event bus (`nats_sub(subject)`) and light up new console surfaces. Design is locked (ADR-023); implementation is on the roadmap. See the [Plugin SDK guide](./docs/plugin-sdk-guide.md).
- **📱 Responsive console** — a polished operator console from desktop to mobile: fleet + project dashboards, live SSE run streams, agents org views, LLM settings with per-runtime adapter tabs, File Explorer, Credentials Vault, and a decision inbox.

## 🖥️ Console

<div align="center">

**Fleet dashboard** — operator overview: fleet health + live agent assignments across every namespace
<img src="docs/media/console-fleet-dashboard.png" alt="KSquad fleet dashboard — operator overview" width="900">

**Project dashboard** — everything scoped to one project, live activity over SSE
<img src="docs/media/console-project-dashboard.png" alt="KSquad project dashboard" width="900">

**Work-item tree** — parent → sub-ticket tree, status, assignee, comments & artifacts
<img src="docs/media/console-tickets-kanban.png" alt="KSquad work-item tree / tickets board" width="900">

**Live run stream** — the coordination record streaming over SSE: checkout → comment → handoff → artifact
<img src="docs/media/console-run-stream.png" alt="KSquad live run stream over SSE" width="900">

**Agents org** — the same squad, organized by role and by leadership, with live status
<img src="docs/media/console-agents-org.png" alt="KSquad agents role org" width="900">

</div>

Beyond the dashboards, the console covers the full operating loop:

- **LLM settings** — per-runtime **adapter tabs** (Claude Code API-key onboarding end-to-end, Vibe full tab, Goose preview) and provider rows including **Mistral**.
- **Credentials Vault** — BYO credentials with save / test / rotate / delete and write/rotate/delete **audit spans**.
- **File Explorer** — browse project workspace PVCs straight from the console.
- **Project detail, fleet-wide** — project health resolved across every squad.
- **GitHub Kanban** — Kanban and list views with sub-tickets, live-run markers, and GitHub automation settings.
- **Decision inbox** — multi-option DecisionCards with letter chips, Recommended badges, and number-key answers.

## 🚀 Quickstart

Get a squad running on any Kubernetes 1.31+ cluster (reproduced in CI by the getting-started smoke workflow):

```bash
# 1. Add the Helm repo and install the operator + console
helm repo add ksquad https://charts.k8squad.io
helm install ksquad ksquad/k8squad --namespace k8squad-system --create-namespace \
  --set modelConfig.default.model=claude-sonnet-4   # REQUIRED: system default model

# 2. Apply the quickstart squad (a Team, an Agent, and a Project)
kubectl apply -f https://charts.k8squad.io/quickstart.yaml

# 3. Open the console
kubectl port-forward -n k8squad-system svc/ksquad-console 8080:80
# → http://localhost:8080
```

See [CONTRIBUTING.md](./CONTRIBUTING.md) to build from source instead.

Want the full pre-defined **BMAD squad** (13 roles in a CEO → PM/Architect/UX
hierarchy, wired with four default Skills) instead of the one-agent demo? Follow
the [Getting Started: the BMAD squad](./docs/getting-started-bmad.md) guide.

## 🏗️ Architecture

KSquad runs a trusted **control plane** (`ksquad-system`) and per-team, untrusted **data planes**.
Kubernetes CRDs are the desired-state API; durable coordination and memory live in a single
Postgres, split into two logically-separated schemas with distinct trust boundaries. The **AgentMesh**
binds the two planes: runtime adapters behind a seam, warm sandboxes scheduled by policy.

```mermaid
flowchart TB
    author(["Operator / Author"]) -->|REST + SSE over HTTPS| console
    kubectl(["kubectl / CRDs"]) --> api

    subgraph cp["Control plane — ksquad-system (trusted, installed once)"]
        console["ksquad-console<br/>Node / Next.js — UI + BFF, SSE fan-out"]
        api["ksquad-apiserver — Go<br/>coordination record · pkg/auth JWT + RBAC<br/>audit query API · SSE progress bus"]
        operator["ksquad-operator<br/>controller-runtime<br/>Team · Agent · Project · Run · RolePool · SandboxPool"]
        memory["ksquad-memory<br/>MCP + pgvector · diary + knowledge graph"]
        pg[("Postgres — single stateful store<br/>coord · memory · auth schemas")]
        console --> api
        api --> operator
        api --> memory
        api --> pg
        operator --> pg
        memory --> pg
    end

    operator -->|creates / tears down| team

    subgraph dp["Data plane — per Team (untrusted, least-privilege, blast-radius-bounded)"]
        subgraph team["Team namespace — RBAC · NetworkPolicy · quota"]
            sandbox["Sandbox Pod<br/>warm-pool · hibernate / rebind-at-wake · gVisor RuntimeClass"]
            shim["shim sidecar<br/>A2A ⇄ native"]
            runtime["agent runtime<br/>claude-code · codex · opencode · openclaw · hermes · vibe · goose · crewai"]
            pvc[("Project workspace PVC<br/>per-principal worktree")]
            sandbox --> shim
            shim <--> runtime
            sandbox --> pvc
        end
    end
```

**Control plane** — operator, API server, memory service, console, and Postgres. Stateful,
cluster-privileged (scoped), installed once. **Data plane** — sandbox pods, shims, agent runtimes,
and workspace PVCs, per `Team`. Untrusted, least-privilege, blast-radius-bounded.

### Custom resources (`ksquad.io/v1alpha1`)

| CRD | Purpose |
|-----|---------|
| `Team` | A squad of agents and a tenancy boundary (namespace, RBAC, quota). |
| `Agent` | One agent instance: its runtime, role, skills, model, and credential references. |
| `AgentRuntime` | A pluggable coding-agent flavor and CLI version policy. |
| `Role` | A set of responsibilities an agent holds within a team. |
| `RolePool` | Warm standing capacity: target-N prescheduling per Role × runtime (scheduling policy #1). |
| `Skill` | A capability an agent exposes. |
| `Project` | A workspace (PVC + source repository). |
| `Run` | A unit of orchestrated work with an A2A task lifecycle. |
| `ModelConfig` | Model tiers and fallback resolution (default tier is the last resort). |
| `MCPServer` | An MCP tool server exposed to agents. |
| `OTelConfig` | Observability pipeline configuration. |
| `EgressPolicy` | Egress control for team namespaces and runtimes. |
| `Toolchain` | The per-runtime toolchain catalog. |

## 🔗 Links

- **Website** — https://k8squad.io
- **Documentation** — https://k8squad.io/docs
- **Getting Started (BMAD squad)** — [docs/getting-started-bmad.md](./docs/getting-started-bmad.md)
- **Supported runtimes** — [docs/supported-agents.md](./docs/supported-agents.md)
- **AgentMesh architecture** — [docs/agent-mesh.md](./docs/agent-mesh.md)
- **Board-working agents** — [docs/board-working-agents.md](./docs/board-working-agents.md)
- **Observability** — [docs/observability/distributed-tracing.md](./docs/observability/distributed-tracing.md)
- **Release notes** — [docs/releases/](./docs/releases/)
- **Contributing** — [CONTRIBUTING.md](./CONTRIBUTING.md)
- **Plugin SDK guide** — [docs/plugin-sdk-guide.md](./docs/plugin-sdk-guide.md)
- **Code of Conduct** — [CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md)
- **Governance** — [GOVERNANCE.md](./GOVERNANCE.md) · [MAINTAINERS.md](./MAINTAINERS.md)
- **Security** — [SECURITY.md](./SECURITY.md)
- **License** — [Apache 2.0](./LICENSE) · [NOTICE](./NOTICE)

## 🤝 Contributing

Contributions are welcome. See **[CONTRIBUTING.md](./CONTRIBUTING.md)** for how to build, test, and
submit pull requests. Please open an issue to discuss substantial changes first.

All commits must be signed off under the **[Developer Certificate of Origin](./DCO.md)**
(`git commit -s`), and all participation is governed by our
**[Code of Conduct](./CODE_OF_CONDUCT.md)**. Project roles and decision-making are described in
**[GOVERNANCE.md](./GOVERNANCE.md)**. To report a security vulnerability, follow
**[SECURITY.md](./SECURITY.md)** — please do not open a public issue.

---

<div align="center">

<img src="docs/media/banner-on-dark.png" alt="K8squad" width="280">

**Apache 2.0 Licensed** · © K8squad contributors

</div>
