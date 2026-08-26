# BMAD team — a runnable K8squad example squad

A self-contained, `kubectl apply`-able squad that models the **BMAD method**
(Breakthrough Method of Agile AI-Driven Development): 13 roles in a reporting
hierarchy, wired with 4 default + 10 dev/debug Skills. Use it to onboard onto K8squad on a real
cluster and as a template for your own squad.

Everything lives in one namespace (`bmad-squad`) and every cross-reference
resolves inside this folder, so apply leaves nothing dangling.

## Prerequisites

The K8squad CRDs and operator must already be installed (Helm chart step 1):

```bash
helm repo add k8squad https://charts.k8squad.io
helm install k8squad k8squad/k8squad
```

CRDs are `ksquad.io/v1alpha1`: `AgentRuntime`, `Agent`, `Role`, `Skill`,
`Team`, `Project`.

## Apply

One file:

```bash
kubectl apply -f squad.yaml
```

…or the numbered files in order (identical result — `squad.yaml` is just their
concatenation):

```bash
kubectl apply -f 00-namespace.yaml \
              -f 01-credentials.yaml \
              -f 02-runtimes.yaml \
              -f 03-skills.yaml \
              -f 04-prompts.yaml \
              -f 05-roles.yaml \
              -f 06-agents.yaml \
              -f 07-project.yaml \
              -f 08-team.yaml
```

**Before pointing it at anything real:** replace `REPLACE_ME` in
`01-credentials.yaml` with a provider token, and set `spec.repo.url` in
`07-project.yaml` to your repository.

Tear down with `kubectl delete namespace bmad-squad`.

## File layout & apply order

| # | File | Contents |
|---|------|----------|
| 00 | `00-namespace.yaml` | `bmad-squad` namespace |
| 01 | `01-credentials.yaml` | model-credential `Secret` (token `REPLACE_ME`) |
| 02 | `02-runtimes.yaml` | `AgentRuntime` (`claude-code`) |
| 03 | `03-skills.yaml` | 4 default + 10 dev/debug `Skill`s |
| 04 | `04-prompts.yaml` | 13 prompt `ConfigMap`s (behavior + hierarchy) |
| 05 | `05-roles.yaml` | 13 `Role`s (promptRef + defaultSkills + labels) |
| 06 | `06-agents.yaml` | 13 `Agent`s (model + role + runtime + credential) |
| 07 | `07-project.yaml` | `Project` (repo the squad works on) |
| 08 | `08-team.yaml` | `Team` (flat composition of agents + project) |
| — | `squad.yaml` | all of the above concatenated |

## The 13 BMAD roles & reporting hierarchy

There is **no manager/`reportsTo` edge in the CRD model** — `Team.spec.agents`
is a flat list. The hierarchy is encoded in each role's **prompt** (04) and
mirrored as `ksquad.io/reports-to` **labels** (05/06), so it is queryable
(`kubectl get roles -l ksquad.io/reports-to=architect`) without a controller
relationship.

```
CEO (sam)
├── Product Manager (john)
│   ├── Brainstormer (mary)
│   ├── Challenger (cade)
│   └── Content Writer (quill)
├── System Architect (winston)
│   ├── Code Reviewer (amelia)
│   ├── Test Architect (tess)
│   ├── Coder (ada)
│   ├── DevOps Engineer (devon)
│   └── Observability Engineer (otto)
└── UX Designer (uma)
    └── Graphical Designer (gabi)
```

## The 4 default Skills

A `Skill` is the CRD-authorized capability envelope. `bmad` ships **inline**
(self-contained); the tool skills are **git-sourced and pinned to a commit SHA**
(`repoRef`/`ref` are placeholders — repoint them at your own skill repo).

| Skill | Source | Granted to | Purpose |
|-------|--------|-----------|---------|
| `bmad` | inline | **all 13 roles** | the BMAD workflow/methodology |
| `github` | git | coder, devops-engineer, code-reviewer | SCM & PR workflows |
| `dynatrace` | git | observability-engineer | telemetry queries (dtctl + Dynatrace MCP) |
| `graphical` | git | graphical-designer | SVG rendering + asset toolchain |

## The 10 dev/debug Skills

Beyond the four defaults, the squad ships ten Skills for **local coding and
on-cluster debugging** (recommended on ISI-3271, added in ISI-3273). Each is a
real `Skill` CR: `source` (inline or SHA-pinned git), least-privilege
`permissions[]` (read-first — no write/apply/delete verbs on the debug skills),
and `requires{toolchains[],sidecars[]}`. Toolchains are standardized on the
repo's real versions (`go@1.25`, `kubectl@1.31`) because version conflicts
across a Run's skills fail closed at pod assembly (arch §5.3.4); the two
`dockerd`-sidecar skills only resolve where `AgentRuntime.capabilities` grants
the capability (§5.3.3).

| Skill | Source | Focus | Granted to |
|-------|--------|-------|-----------|
| `code-search` | git | dev | coder, code-reviewer, architect, test-architect |
| `kubectl-debug` | inline | debug | coder, test-architect, devops-engineer, observability-engineer |
| `go-build-test` | inline | dev+debug | coder, code-reviewer, test-architect |
| `git-workflow` | inline | dev+debug | coder, code-reviewer, test-architect, devops-engineer |
| `golangci-lint` | inline | dev | coder, code-reviewer |
| `otel-observability-query` | git | debug | coder, test-architect, devops-engineer, observability-engineer |
| `container-build` | inline | dev+debug | coder, devops-engineer |
| `delve-pprof` | inline | debug | coder, observability-engineer |
| `psql-inspect` | inline | debug | coder, devops-engineer, test-architect |
| `http-grpc-probe` | inline | debug | coder, test-architect, devops-engineer |

**How they attach.** Broad-value skills (attached to ≥3 roles) attach via
`Role.spec.defaultSkills[]`. The three specialist skills (`golangci-lint`,
`container-build`, `delve-pprof`) attach per-member via `Agent.spec.skillRefs[]`.
Because `skillRefs` **replaces** a role's `defaultSkills` (it is an override, not
a merge), the four members that use it — `amelia`, `ada`, `devon`, `otto` —
enumerate their **complete** skill set there, so none of them loses `bmad`,
`github`, or `dynatrace`. Every other member inherits purely from its role
defaults. The most tool-rich member is the Coder (`ada`), who carries all ten.

## Customizing

- **Model per role** — `Agent.spec.model` (opus for CEO/PM/Architect, sonnet for
  the rest here).
- **Credential kind** — set `Agent.spec.credentialClass: human-seat` to inject an
  interactive OAuth token instead of the default service-account API key.
- **Sandbox posture** — execution roles set `Role.spec.runtimeClassHint: gvisor`.
- **Add a role** — add a prompt ConfigMap (04), a Role (05), an Agent (06), then
  list the agent in the Team (08). Keep the `ksquad.io/reports-to` label
  consistent with the prompt.

> Getting-Started guide authors: the filenames and apply order above are the
> stable contract. If you change them, update this table and notify downstream.

## Validation

Every object validates against the shipped CRD OpenAPI schemas and all `*Ref`
targets resolve inside the folder. To re-check against a live cluster:

```bash
kubectl apply -f squad.yaml --dry-run=server
```
