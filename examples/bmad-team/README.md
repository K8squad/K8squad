# BMAD team — a runnable K8squad example squad

A self-contained, `kubectl apply`-able squad that models the **BMAD method**
(Breakthrough Method of Agile AI-Driven Development): 13 roles in a reporting
hierarchy, wired with 7 default Skills. Use it to onboard onto K8squad on a real
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

`02b-mcpservers.yaml` is **optional** and intentionally left out of the apply
above — the default skills reach their tools through CLI toolchains and need
no MCPServer. Apply it only if you add an MCP-backed skill (see *The
capability plane* below).

**Before pointing it at anything real:** replace the `REPLACE_ME` model
provider token in `01-credentials.yaml` (the `github-mcp` / `dynatrace-mcp`
token Secrets are only needed if you opt into `02b-mcpservers.yaml`), set
`spec.repo.url` in `07-project.yaml` to your repository, and make sure the
toolchain catalog is enabled (see *The capability plane* below).

Tear down with `kubectl delete namespace bmad-squad`.

## File layout & apply order

| # | File | Contents |
|---|------|----------|
| 00 | `00-namespace.yaml` | `bmad-squad` namespace |
| 01 | `01-credentials.yaml` | model-credential `Secret` (+ optional MCP token `Secret`s for 02b) (`REPLACE_ME`) |
| 02 | `02-runtimes.yaml` | `AgentRuntime` (`claude-code`) |
| 02b | `02b-mcpservers.yaml` | **optional** — an `MCPServer` reference example (not used by the default skills) |
| 03 | `03-skills.yaml` | the 7 default `Skill`s |
| 04 | `04-prompts.yaml` | 13 prompt `ConfigMap`s (behavior + hierarchy) |
| 05 | `05-roles.yaml` | 13 `Role`s (promptRef + defaultSkills + labels) |
| 06 | `06-agents.yaml` | 13 `Agent`s (model + role + runtime + credential) |
| 07 | `07-project.yaml` | `Project` (repo the squad works on) |
| 08 | `08-team.yaml` | `Team` (flat composition of agents + project) |
| — | `squad.yaml` | all of the above concatenated |

## The capability plane behind these Skills (ISI-3280, Epics A-C)

The tool Skills are not self-contained — their `requires.toolchains` resolve
against the cluster `Toolchain` catalog this example expects on the cluster
(`MCPServer` is the second, optional object kind, unused by the defaults):

- **`Toolchain` (the cluster catalog)** — `03-skills.yaml` requires `gh@2.98`,
  `dtctl@1.0` and `node@22` as `name@version` refs. Enable the Helm chart's
  default catalog when installing the operator:

  ```bash
  helm install k8squad config/helm -n k8squad-system --create-namespace \
    --set tools.defaultCatalog.enabled=true
  ```

  That renders the curated fourteen-tool catalog (`kubectl`, `git`, `gh`,
  `go`, `node`, `dtctl`, `helm`, `python`, `docker-cli`, `uv`, `jq`, `yq`,
  `curl`, `make`) as `Toolchain` objects in `k8squad-system`, with
  least-privilege RBAC per tool (kubectl: read-only core+apps; the rest:
  staged onto PATH with no Kubernetes API grant). `docker-cli` is the
  client only — the daemon stays the `dockerd` sidecar. Run assembly stages each
  resolved toolchain as an init container and unions its RBAC into a per-Run
  `Role` bound to the squad ServiceAccount — garbage-collected with the Run.
  Unknown name/version fails Run admission with an actionable message.

- **`MCPServer` (optional, `02b-mcpservers.yaml`)** — none of the default
  skills use it: `github` drives the `gh` CLI and `dynatrace` drives `dtctl`,
  and a CLI tool needs no MCPServer. The file is kept as a reference example
  for the case where you add a skill whose tool surface is reachable **only**
  over MCP. If you do: the skill gets an `mcpToolRefs` entry, the discovery
  controller probes the server (initialize → tools/list) and populates
  `status.observedTools` (Runs fail closed while the surface is unknown, so
  give it a probe cycle), you supply the token Secret (01), and a Skill may
  only **narrow** the server's `toolFilter`, never widen it (trust boundary
  D8).

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

## The 7 default Skills

A `Skill` is the CRD-authorized capability envelope. `bmad`, `task-io`,
`org-ops` and `project-ops` ship **inline** (self-contained); the tool skills
are **git-sourced and pinned to a commit SHA** (`repoRef`/`ref` are placeholders
— repoint them at your own skill repo).

| Skill | Source | Granted to | Purpose |
|-------|--------|-----------|---------|
| `bmad` | inline | **all 13 roles** | the BMAD workflow/methodology |
| `task-io` | inline | **all 13 roles** | read/comment/status/checkout on your *own* task (ISI-3602) |
| `org-ops` | inline | ceo, product-manager, architect, ux-designer | create agents & skills, delegate work (ADR-0005) |
| `project-ops` | inline | ceo | create & archive projects (ADR-0005) |
| `github` | git | coder, devops-engineer, code-reviewer | SCM & PR workflows |
| `dynatrace` | git | observability-engineer | telemetry queries (dtctl + Dynatrace MCP) |
| `graphical` | git | graphical-designer | SVG rendering + asset toolchain |

`task-io` is the **union baseline**: every working agent gets it via its Role so
it can act on its own work item on demand (the coord-side analogue of an issue
read/write API, delivered by S2/ISI-3601). It grants *agency over your own task*,
never a general issue-browser and never context assembly — task context is PUSHED
into the prompt at boot (S1/ISI-3590). It ships inline until S2 finalizes the
coord API wire shapes, then converts to a SHA-pinned catalog entry.

### Board-ops (`org-ops` / `project-ops`) — the security boundary is the token, not attachment

`org-ops` and `project-ops` are **privileged** board-ops skills (ADR-0005): they
let CEO/manager roles grow the squad by driving the run-scoped org-ops seam
(`/api/org-ops/{create-agent,create-skill,create-project,archive-project}`,
ISI-3626). They ship **inline with executable tool definitions** — concrete
HTTP method/path/schema an agent can call, carrying the injected
`KSQUAD_COORD_TOKEN`, plus an A2A descriptor for delegating work to a teammate.

The critical rule (**ADR-0005 D2**): **attaching one of these skills grants no
authority.** The real gate is the run-scoped token, whose `org:write` /
`project:write` scope the coord derives from the role's position in the
`ksquad.io/reports-to` graph (`pkg/orgops` `DeriveScopes`): a **manager** (a
reports-to target) gets `org:write`; the **CEO** (a manager reporting to no one)
also gets `project:write`; an **IC/leaf** role gets neither and every board-ops
call is rejected server-side. Attachment in `05-roles.yaml` is restricted to
match the graph purely as advertisement — a leaf role that somehow carried the
skill would still be denied. Scopes are coarse (`org:write` covers create-agent
*and* create-skill; there are no finer `agent:create`/`skill:create` scopes), and
no permission is ever self-widened by the skill body (trust boundary D8).

### Context is PUSHED at boot — do not add a read-project/read-issue skill

Every agent on this squad receives its task context (title, description,
acceptance criteria, comments, goal) **pushed into its prompt at Run start** by
`contextasm` (S1/ISI-3600) — the K8squad model is PUSH, unlike Paperclip's PULL.
When templating your own squad, do **not** add a redundant `read-project` /
`read-issue` skill to fetch context at boot; it was explicitly rejected in the
ISI-3588 study (§4/§6). `task-io` covers the only legitimate post-boot need
(re-reading *your own* changed work item on demand), and `org-ops`/`project-ops`
cover growing the org — governed by the role-scoped token (D2), not by
attachment.

Skills attach to roles via `Role.spec.defaultSkills[]`; every Agent inherits its
role's defaults as a **union** (`Agent.spec.skillRefs ∪ Role.spec.defaultSkills`,
ADR-044 / ISI-3591), so an agent that sets its own `skillRefs` still keeps every
role default including `task-io`.

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

Every object validates against the shipped CRD OpenAPI schemas, and all
in-folder `*Ref` targets resolve inside the folder. The one cluster-level
expectation for the default skills (documented above): `requires.toolchains`
names must exist in the `Toolchain` catalog before Runs admit. (If you opt
into the optional `02b-mcpservers.yaml`, its `mcpToolRefs` must also have
discovered their tool surface first.) To re-check against a live cluster:

```bash
kubectl apply -f squad.yaml --dry-run=server
```
