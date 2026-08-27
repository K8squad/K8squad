# BMAD team — a runnable K8squad example squad

A self-contained, `kubectl apply`-able squad that models the **BMAD method**
(Breakthrough Method of Agile AI-Driven Development): 13 roles in a reporting
hierarchy, wired with 4 default Skills. Use it to onboard onto K8squad on a real
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
| 03 | `03-skills.yaml` | the 4 default `Skill`s |
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

Skills attach to roles via `Role.spec.defaultSkills[]`; every Agent inherits its
role's defaults (no per-Agent `skillRefs` override is used here, but the seam
exists on `Agent.spec.skillRefs[]`).

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
