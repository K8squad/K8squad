# KSquad

**Kubernetes-native, agent-agnostic AI agent orchestration platform.**

KSquad organizes AI agents into **squads** — virtual teams that coordinate on shared
work items rather than ad-hoc peer-to-peer chat. It is an operator-based platform:
agents, teams, roles, skills, projects, and runs are all first-class Kubernetes
custom resources, reconciled by controllers.

> Status: early development. Interfaces and CRDs are not yet stable.

## Highlights

- **Squad coordination model.** Agents collaborate through shared work items
  (issues, comments, checkout) instead of direct agent-to-agent messaging.
- **Agent-agnostic runtimes.** Heterogeneous agent runtimes are supported through
  thin shims, so squads can mix agent implementations.
- **A2A southbound protocol.** Agent Cards for capability discovery, task lifecycle
  for runs, artifacts for handoffs, and SSE for streaming progress.
- **MCP for tools.** Agents reach tools through the Model Context Protocol.
- **First-class memory server.** Shared memory is a built-in component of the
  platform, not an external dependency.
- **BYO-subscription credentials.** Per-user credential Secret references live on
  the `Agent` custom resource.

## Architecture at a glance

| Layer | Technology |
|-------|------------|
| Backend / control plane | Go (Kubernetes operators + API) |
| Frontend | Node.js |
| Southbound protocol | A2A (Agent Cards, task lifecycle, artifacts, SSE) |
| Tool protocol | MCP |
| Memory | First-class memory server |

### Custom resources

The platform surface is expressed as Kubernetes CRDs:

- `Team` — a squad of agents working a shared backlog.
- `Agent` — a single agent, its runtime shim, and its credential references.
- `Role` — a set of responsibilities an agent can hold within a team.
- `Skill` — a capability an agent exposes.
- `Project` — a workspace (PVC + source repository).
- `Run` — a unit of orchestrated work with a task lifecycle.

## Repository layout

Source scaffolding lands here as it is created (Go control plane, Node.js frontend,
CRD definitions, and deployment manifests).

## Contributing

Contributions are welcome. See **[CONTRIBUTING.md](./CONTRIBUTING.md)** for how to
build, test, and submit pull requests. Please open an issue to discuss substantial
changes before submitting a pull request.

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](./LICENSE).
