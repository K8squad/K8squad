# KSquad Governance

This document describes the governance model for the KSquad project. It is
intentionally lightweight for the project's current early stage and will be
revised as the community grows. It is written to satisfy the governance and
scope expectations of a [CNCF Sandbox](https://github.com/cncf/sandbox)
application.

## Scope

**Mission.** KSquad is a Kubernetes-native, agent-agnostic platform for
orchestrating AI agents as coordinated *squads* — virtual teams that collaborate
through shared work items rather than ad-hoc peer-to-peer messaging.

**In scope**

- Kubernetes custom resources and controllers for agents, teams, roles, skills,
  projects, and runs (the operator).
- The coordination spine: shared work items, checkout/lease/fencing semantics,
  and run lifecycle.
- Agent-agnostic runtime integration via thin shims.
- Southbound protocols for agent interoperability (A2A) and tool access (MCP).
- Reference API server, memory service, and console for operating squads.

**Out of scope**

- Building or shipping a specific proprietary agent model or runtime.
- Hosted/managed SaaS operation of KSquad.
- General-purpose workflow engines unrelated to agent coordination.

## Roles

- **Contributors** — anyone who files an issue or opens a pull request. All
  contributions require a Developer Certificate of Origin sign-off (see
  [DCO.md](DCO.md)).
- **Maintainers** — contributors with write access who review and merge changes,
  triage issues, and steward releases. The current maintainers are listed in
  [MAINTAINERS.md](MAINTAINERS.md).

## Decision-making

The project operates by **lazy consensus**: proposals (issues or pull requests)
that receive no sustained objection within a reasonable review window are
accepted. Substantive or contested changes require approval from at least one
maintainer other than the author. Maintainers seek consensus; if consensus
cannot be reached, a simple majority vote of maintainers decides.

Changes to architecture-critical code (the coordination spine, public CRD
schemas, security-relevant paths) require maintainer review and must pass the
required status checks, including the coordination-spine chaos suite.

## Becoming a maintainer

Contributors who have shown sustained, high-quality involvement — reviews,
merged contributions, and good judgment — may be nominated by an existing
maintainer. Confirmation requires lazy consensus of the current maintainers. New
maintainers are added to [MAINTAINERS.md](MAINTAINERS.md) by pull request.

## Changing this document

Amendments to this governance document follow the same lazy-consensus process
and require approval from a majority of maintainers.

## Code of conduct

All participants are expected to follow the
[Code of Conduct](CODE_OF_CONDUCT.md).

## Licensing

KSquad is licensed under the [Apache License 2.0](LICENSE). Contributions are
accepted under the same license and must be signed off per the DCO.
