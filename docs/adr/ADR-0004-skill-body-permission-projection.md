# ADR-0004: Skill body + permission projection into the agent runtime

- Status: Accepted (design), rev. 2 — ISI-3604
- Date: 2026-09-02 (rev. 2 incorporates ISI-3610 feasibility review)
- Author: Winston (System Architect)
- Related: ISI-3602 (proves the need), ISI-3591 / ADR-044 (skill union), ISI-3600
  (contextasm→dispatch env seam), ISI-3601 (task-io coord API/env),
  ISI-3610 (feasibility review — F1/F2 fixes below), ISI-3609 (story slicing),
  arch §5.3.4 / §5.3.6 (skill source + staging), D8 trust boundary.

> **Rev. 2 (post-review).** The ISI-3610 feasibility review confirmed every code
> claim and the three-seam split, and surfaced two blocking corrections now folded
> in: **F1** — Phase 1 is *advertise-only*, never deny (the opaque
> `spec.permissions` vocabulary has no runtime-native mapping yet, so a deny rule
> would block *all* default skills including task-io); the mapping table + hard
> deny move wholly to Phase 2. **F2** — the "admission-bounded" premise was false
> (`source.inline` had no `maxLength`); delivery is now **one ConfigMap per skill**
> *and* a real `maxLength` is added at CRD + apiserver. **F3** — skills ride as
> `ExecSpec.WorkDirFiles` from `Command` + an extended `LaunchContext`, not a new
> `Runtime` interface method. **F4** — inline/git split kept (Phase 1 is inline-only,
> no dual-path cost).

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
`SkillsMountPath = "/ksquad/skills"`.

Each granted skill lands at `${KSQUAD_SKILLS_DIR}/<name>/` as:
- `SKILL.md` (or the body's native entry file) — the body bytes;
- `permissions.json` — the CRD-authorized `spec.permissions`, **written by the
  reconciler from the CR**, never from body content (D8: the body cannot
  author its own envelope).

- **Inline bodies** → **one projected ConfigMap per skill**,
  `ksquad-run-<run>-skill-<name>`, mounted at that skill's `<name>/` subdir. Per
  the ISI-3610 review (**F2**), a *single* aggregate ConfigMap risks the 1 MiB
  ConfigMap ceiling once several default skills are enabled, and the earlier
  "admission-bounded" premise was false — `source.inline` had **no `maxLength`**
  in the CRD or apiserver. Rev. 2 therefore (a) adds a real `maxLength` on
  `source.inline` at both the **CRD** (`config/crd/bases/ksquad.io_skills.yaml`)
  and the **apiserver validate** path (`internal/apiserver/composecrd.go`), sized
  so any single skill stays well under 1 MiB, **and** (b) uses one ConfigMap per
  skill so bodies never aggregate. Per-skill also matches the `<name>/` dir layout
  and mirrors the MCP IR carrier one-to-one. Operator-spawned path materializes
  each to temp files, cloning `materializeMCPConfig`.
- **Git bodies** → one hardened init container per git-sourced skill that
  fetches at the **pinned SHA** via `pkg/scm` and stages into the shared skills
  emptyDir — the `RenderInitContainers` toolchain pattern. This keeps large,
  **untrusted** (D8) content out of etcd and out of the control-plane process,
  and the SHA pin gives §5.3.6 reproducibility.

### 3. Projection + enforcement — `pkg/shim`

The shim reads `KSQUAD_SKILLS_DIR` at startup (mirroring `LoadMCPConfig`) into
`Config.GrantedSkills`, and in `drive`/launch:
- projects each body into the runtime's **native** skills location. Per the
  ISI-3610 review (**F3**), this rides the *existing* MCP seam, **not** a new
  interface method: the `Runtime` interface exposes only
  `Command(lc) (ExecSpec, error)`, and MCP already renders inside `Command` by
  emitting `ExecSpec.WorkDirFiles` (cf. `opencode.go` `mcpWorkDirFile`). Skills +
  the native permission file therefore ride as **additional `WorkDirFiles`
  returned from `Command`**, with `LaunchContext` extended by `GrantedSkills`.
  No new method → no churn across the three adapters or the §5.6 conformance
  suite. (Stories must confirm `WorkDirFile.Name` accepts nested paths, e.g.
  `.claude/settings.json`.)
- **Phase 1 is advertise-only.** Per the ISI-3610 review (**F1**): every default
  skill declares `spec.permissions` in an opaque namespaced vocabulary
  (`scm:read`, `workflow:bmad`, `issue:write`, …) with **no mapping** to any
  runtime-native allow-list today (Claude Code's vocabulary is tool-shaped:
  `Bash(...)`, `Read`, `Edit`). A deny-on-unmappable rule in Phase 1 would deny
  projection of *all* default skills — including `task-io` — defeating the
  Phase-1 goal. So Phase 1 renders the body and **advertises the raw effective
  envelope on the Agent Card**, and never gates projection on permission
  honoring. The `permissions`→native allow-list **mapping table** and the
  fail-closed **deny/hard-enforcement** rule move wholly to Phase 2.

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

**Phase-2 carrier split for git skills (ISI-3610 D8 note).** For a git-sourced
skill the body arrives via the init-container **emptyDir** (untrusted), while its
`permissions.json` must arrive via a **trusted** carrier — a reconciler-written
projected ConfigMap — even though both land in the same `<name>/` dir. Phase-2
stories must **not** let the init container write `permissions.json`; if the
trusted `permissions.json` is absent the shim treats the envelope as empty, not
as whatever the fetched tree happens to contain.

## Why this split (rejected alternatives)

- **Projection entirely in the reconciler.** Rejected: the runtime-native skills
  format and permission model are runtime-specific; only the shim (which owns the
  runtime adapter) can render them. The reconciler's job stops at delivering
  bytes + envelope to a known path — the exact MCP split.
- **Inline bodies via init container too (single carrier).** Rejected for
  Phase 1: inline bodies are already in etcd and are size-bounded by the new
  `source.inline` `maxLength` (rev. 2, F2); a projected per-skill ConfigMap is
  simpler and needs no `pkg/scm` fetch. Git needs the init container for size +
  trust; inline does not.
- **Single aggregate skills ConfigMap.** Rejected in rev. 2 (F2): aggregating all
  granted inline skills into one `ksquad-run-<run>-skills` ConfigMap risks the
  hard 1 MiB ConfigMap ceiling once several default skills are enabled, and there
  was no admission size cap to bound it. One ConfigMap per skill removes the
  aggregation entirely and mirrors the per-`<name>/` dir layout.
- **Git bodies in a ConfigMap (reconciler fetches, writes to etcd).** Rejected:
  puts untrusted external content (D8) and potentially >1 MiB bodies into etcd
  and the control-plane process. Init-container fetch keeps it in the sandbox.

## Phasing

- **Phase 1 (unblocks task-io + all five default skills — all currently inline):**
  collection (`GrantedSkill`); `source.inline` `maxLength` at CRD + apiserver (F2);
  inline-body **per-skill** ConfigMap projection end-to-end; shim native
  projection via `WorkDirFiles` (F3) + Agent Card **advertise-only** envelope
  (F1 — no deny, no mapping table); inline content-hash on `skill.load`.
- **Phase 2 (follow-on):** git-source init-container fetch at pinned SHA
  (§5.3.6) with the trusted-`permissions.json` carrier split (D8 note); the
  `permissions`→runtime-native allow-list **mapping table**; and hard permission
  enforcement / deny-on-unmappable / sandboxing beyond advertisement.

Phase 1 alone makes S3's `task-io` functional end-to-end, since every default
skill ships an inline body today.
