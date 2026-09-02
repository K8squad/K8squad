# ADR-0004: Skill body + permission projection into the agent runtime

- Status: Accepted (design) — ISI-3604
- Date: 2026-09-02
- Author: Winston (System Architect)
- Related: ISI-3602 (proves the need), ISI-3591 / ADR-044 (skill union), ISI-3600
  (contextasm→dispatch env seam), ISI-3601 (task-io coord API/env),
  arch §5.3.4 / §5.3.6 (skill source + staging), D8 trust boundary.

## Context

Skill CRs are currently **inert as capability-granting bodies**. Verified in code:

- `pkg/capability/requirements.go` (`Collect`) walks the effective skill set
  correctly — `Agent.spec.skillRefs ∪ Role.spec.defaultSkills` (ADR-044 step 1,
  lines 95–108) — but consumes **only** `spec.requires.toolchains`,
  `spec.mcpToolRefs`, and `spec.requires.sidecars`. It never reads
  `spec.source.inline`, fetches `spec.source.git`, or reads `spec.permissions`.
- The shim (`pkg/shim/engine.go`) uses `Config.Skills` (names only) to (a) emit
  `skill.load` telemetry and (b) advertise skill **names** on the Agent Card
  (§6.1). It never projects a body into the runtime and never enforces the
  permission envelope. The struct comment even notes "Inline bodies have no
  entry" in `SkillSHAs`.
- The reconciler (`pkg/controller/rundrive/dispatch.go`, `shimCommand`) injects
  `KSQUAD_SKILLS` (names) and `KSQUAD_SKILL_SHAS` (pin map) — **no bodies**.
- The only reader of `source.inline` / `permissions` is
  `internal/apiserver/composecrd.go` — the CR create/validate path, not runtime
  projection.

**Consequence:** declaring a Skill on a Role or Agent does not deliver its body
to the runtime nor enforce its `permissions`. This affects **every** default
skill (bmad, github, dynatrace, graphical, task-io) equally. It is pre-existing,
not introduced by ISI-3602; the `task-io` CR (PR #236) is correct and simply
cannot light up until this projection path exists.

## The template already in the tree

MCP capability projection is the proven pattern to copy — three clean layers,
one per concern:

1. **Union in `pkg/capability`** — `Collect` folds the effective skill set into
   a `Requirements` envelope.
2. **Render + deliver (reconciler + `pkg/capability/staging.go`)** — the IR is
   delivered as a projected ConfigMap (`ksquad-run-<name>-mcp`) mounted at
   `/ksquad/mcp/config.json`, named by env `K8SQUAD_MCP_CONFIG`. The
   operator-spawned path materializes it to a temp file at the *same* env path
   (`materializeMCPConfig`). Toolchain bodies use the sibling pattern: one
   hardened init container per toolchain stages bytes into a shared emptyDir
   (`RenderInitContainers`).
3. **Native render in `pkg/shim`** — the shim loads `K8SQUAD_MCP_CONFIG` once at
   startup (`capability.LoadMCPConfig`) and the runtime **adapter** renders the
   runtime-native config.

Skill bodies + permissions get exactly this shape.

## Decision

Land the projection as a **three-part seam, each part in the layer that already
owns the analogous concern**. No new subsystem.

### 1. Collection — `pkg/capability` (`Collect`)

Extend `Requirements` with `Skills []GrantedSkill`, collected in the **same walk**
that already unions toolchains/MCP/sidecars — so union-correctness
(`skillRefs ∪ defaultSkills`, dedup by ns/name, first-seen order) is inherited
for free and Role-default skills like `task-io` reach the runtime by construction.

```go
type GrantedSkill struct {
    Namespace   string
    Name        string
    SourceType  string          // "inline" | "git"
    Inline      string          // when SourceType==inline
    Git         *GitSkillSource // repoRef + pinned Ref(SHA) + path, when git
    Permissions []string        // CRD-authorized envelope (D8 authority)
}
```

Fail-closed posture is unchanged: a skill that resolves contributes its body +
envelope; read failures remain errors.

### 2. Delivery — reconciler + `pkg/capability/staging.go`

Two carriers, one destination directory. Add skill-projection constants beside
the MCP ones in `staging.go`: `SkillsDirEnvVar = "KSQUAD_SKILLS_DIR"`,
`SkillsMountPath = "/ksquad/skills"`, volume `ksquad-run-<name>-skills`.

Each granted skill lands at `${KSQUAD_SKILLS_DIR}/<name>/` as:
- `SKILL.md` (or the body's native entry file) — the body bytes;
- `permissions.json` — the CRD-authorized `spec.permissions`, **written by the
  reconciler from the CR**, never from body content (D8: the body cannot
  author its own envelope).

- **Inline bodies** → projected ConfigMap `ksquad-run-<name>-skills`, exactly
  like the MCP IR (bounded by admission; already in etcd). Operator-spawned path
  materializes to temp files, cloning `materializeMCPConfig`.
- **Git bodies** → one hardened init container per git-sourced skill that
  fetches at the **pinned SHA** via `pkg/scm` and stages into the shared skills
  emptyDir — the `RenderInitContainers` toolchain pattern. This keeps large,
  **untrusted** (D8) content out of etcd and out of the control-plane process,
  and the SHA pin gives §5.3.6 reproducibility.

### 3. Projection + enforcement — `pkg/shim`

The shim reads `KSQUAD_SKILLS_DIR` at startup (mirroring `LoadMCPConfig`) into
`Config.GrantedSkills`, and in `drive`/launch:
- projects each body into the runtime's **native** skills location via a
  per-runtime adapter method (the same split as MCP native rendering — only the
  adapter knows the runtime's on-disk skills layout);
- applies each skill's `permissions.json` to the runtime's native permission
  mechanism where one exists (e.g. Claude Code settings allow/deny lists), and
  **advertises the effective envelope on the Agent Card**. Fail-closed: a skill
  whose declared permissions the runtime cannot honor is **denied projection**,
  not silently granted.

The existing `skill.load` telemetry stays; `SHA256` now populates for git skills
from the pinned ref, and inline bodies get a content hash so `skill.load` spans
carry a real integrity anchor for all skills (closes the "inline bodies have no
entry" gap).

## Trust boundary (D8)

The **only** authority for a skill's capability envelope is
`Skill.spec.permissions`, authored by the operator/admin who registered the CR.
The body — inline or git-fetched — is data. It supplies behavior *inside* the
envelope and never widens it. The shim computes the envelope from the projected
`permissions.json`, never by parsing anything inside the body. Git bodies remain
untrusted-external (consistent with `pkg/scm` mirror trust levels).

## Why this split (rejected alternatives)

- **Projection entirely in the reconciler.** Rejected: the runtime-native skills
  format and permission model are runtime-specific; only the shim (which owns the
  runtime adapter) can render them. The reconciler's job stops at delivering
  bytes + envelope to a known path — the exact MCP split.
- **Inline bodies via init container too (single carrier).** Rejected for
  Phase 1: inline bodies are already in etcd and admission-bounded; a projected
  ConfigMap is simpler and needs no `pkg/scm` fetch. Git needs the init
  container for size + trust; inline does not.
- **Git bodies in a ConfigMap (reconciler fetches, writes to etcd).** Rejected:
  puts untrusted external content (D8) and potentially >1 MiB bodies into etcd
  and the control-plane process. Init-container fetch keeps it in the sandbox.

## Phasing

- **Phase 1 (unblocks task-io + all five default skills — all currently inline):**
  collection (`GrantedSkill`), inline-body ConfigMap projection end-to-end,
  shim native projection + Agent Card permissions advertisement + runtime-native
  allow-list mapping where available, inline content-hash on `skill.load`.
- **Phase 2 (follow-on):** git-source init-container fetch at pinned SHA
  (§5.3.6), and hard permission enforcement/sandboxing beyond advertisement.

Phase 1 alone makes S3's `task-io` functional end-to-end, since every default
skill ships an inline body today.
