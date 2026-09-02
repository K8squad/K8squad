# ADR-0005: Board-ops skill family (task-io, org-ops, project-ops) and role-gated capability projection

- Status: Accepted (design) — ISI-3624
- Date: 2026-09-02
- Author: Winston (System Architect)
- Related: ISI-3588 (agent-bootstrap study — PULL vs PUSH), ISI-3590 S3 /
  ISI-3602 (task-io skill), ISI-3601 (coord task-io API/token seam),
  ISI-3600 (contextasm boot-time context PUSH), ISI-3591 / ADR-044 (skill
  union), ADR-0004 (skill body + permission projection seam),
  arch §5.1 (Role) / §5.3.6 (default skills).

## Context

The board asked (local-board on ISI-3591) for the reference squad's agents to
actually **operate the board** — create tickets, comment, assign, and for
privileged roles, create agents/skills and projects. That request splits into
three capability tiers, and only the third is genuinely new design work:

1. **Read your own task context** — already delivered as a boot-time **PUSH**
   via `contextasm` (ISI-3600). The agent does not fetch its context; the
   platform assembles and injects it when the Run starts (ISI-3588 study §8.5).
   No new skill; the reference squad only needs to **document** that this is the
   default so squad authors do not add a redundant "read-project" skill.
2. **Act on your own task on demand** (re-read, comment, status, checkout) —
   delivered as the **task-io** skill (ISI-3602, wired at every Role's
   `defaultSkills[]` in `examples/bmad-team`, PR #236 stacked on the coord seam
   PR #237). Union-baseline, ungated: every working agent gets it. Its security
   boundary is a **run-scoped coord token** bound to `RUN_ID` + `WORK_ITEM_ID`
   (ISI-3601) — the token, not the skill body, is the enforcement.
3. **Operate the organization** — create agents + skills (privileged), create
   projects (most privileged). **New, and this ADR's subject.** These must be
   **role-gated** so ICs cannot self-serve org changes.

The through-line: a *skill* is how an agent is told it may drive a coord/org API;
the *coord/org API* (via a role-scoped token) is where the boundary is actually
enforced. task-io proved this shape. org-ops/project-ops extend it to privileged
verbs, and therefore must add a **real** server-side gate — advertise-only
(ADR-0004 Phase 1) is not sufficient for privileged capability.

## Decision

### D1 — org-ops and project-ops are Skills, not runtime capabilities

Model both as **skills over a coord org/project API** — the same skill-as-API-
driver shape as task-io — not as a new runtime feature or a bespoke agent
capability flag. Rationale:

- Consistency: task-io already establishes "skill body = how to drive a
  run-scoped coord API." Reusing it means org-ops/project-ops ride the exact
  ADR-0004 projection seam (per-skill inline ConfigMap → `WorkDirFiles` →
  runtime-native skills dir) with zero new machinery.
- The board API (create agents/skills/projects) is plain HTTP, like the coord
  task-io API — no CLI toolchain is required (a `requires.toolchains` line is
  added only if a future client needs a CLI).
- Runtime-capability modeling would fork a second authorization path parallel to
  skills; ADR-0004 already gives us one. Boring wins.

Two skills (not one), because their blast radius and gating differ:

| Skill | Verbs | Granted to |
|-------|-------|-----------|
| `org-ops` | create-agent, create-skill (register a Skill CR), assign-work across the org | CEO **and** manager roles |
| `project-ops` | create-project, archive-project | CEO **only** |

("manager role" = any Role that is a `ksquad.io/reports-to` **target** — in the
BMAD squad: CEO, Product Manager, Architect, UX Designer. This is queryable
without a controller: `kubectl get roles -l ksquad.io/reports-to=<name>` is
non-empty ⇒ `<name>` is a manager. ICs are the leaf roles.)

### D2 — the permission boundary is enforced server-side, keyed on the run's role, not by the skill body and not by advertise-only skill permissions

This is the load-bearing decision. Three candidate enforcement points, and where
each actually lives:

1. **Skill body** — REJECTED as an authority (ADR-0004 D8). The body is data;
   it can *describe* org-ops but must never *authorize* it. An agent that
   fabricates or edits an org-ops body must still be denied.
2. **Skill `spec.permissions` advertise-only** (ADR-0004 Phase 1) — NECESSARY
   BUT NOT SUFFICIENT. Phase 1 renders bodies and advertises the raw envelope on
   the Agent Card **without denying** — there is no runtime-native deny yet. For
   task-io that is safe (the coord token self-limits to one work item). For
   org-ops/project-ops it is **not** safe: advertise-only would let any agent
   holding the body call the org API. So we do **not** rely on Phase 1
   enforcement for these.
3. **Coord/org API, gated on a role-scoped token** — THE ENFORCEMENT POINT.
   The Run's injected token (the task-io `KSQUAD_COORD_TOKEN` lineage, ISI-3601)
   must carry the **role-derived scope**: `org:write` for CEO+manager runs,
   `project:write` for CEO runs, neither for IC runs. The org/project API checks
   the token scope on every call and rejects out-of-scope requests server-side —
   exactly as the task-io coord API rejects cross-run access today. This holds
   even if an IC agent somehow obtained the skill body: its run token lacks the
   scope, so the API says no.

**So there are two gates, and they do different jobs:**

- **Attachment gate (ergonomic):** `Role.spec.defaultSkills[]` lists `org-ops`
  only on CEO+manager Roles and `project-ops` only on the CEO Role. This keeps
  the capability *out of ICs' sight and prompt* — they are never told they can,
  so they don't try. Because skills union (`skillRefs ∪ defaultSkills`, ADR-044),
  attachment alone is **not** a security boundary: an Agent could in principle
  set `skillRefs: [org-ops]` on itself.
- **Authorization gate (security):** the role-scoped coord token. This is the
  real boundary and it does not depend on attachment. An IC that self-adds
  `org-ops` gets the body but a token with no `org:write` scope → API denies.

Deriving the token scope from the **Role** (not the Agent, not the skill) means
the gate cannot be widened by per-Agent `skillRefs`, closing the union loophole.

### D3 — sequencing (what exists vs what must be built)

- **Now (documentation only):** the reference squad README documents the
  `contextasm` context PUSH default (tier 1) so squad authors do not add
  redundant read skills. Fold this into the same change that lands the
  org-ops/project-ops CRs (below), on top of the PR #237/#236 merge, to avoid a
  README conflict with #236's task-io edits.
- **Prerequisite (new coord/apiserver work — must be built before the skills do
  anything):** a coord org/project API and **role-scoped token minting** that
  stamps `org:write` / `project:write` into the Run token per D2. This extends
  the ISI-3601 coord token seam. Until it exists, org-ops/project-ops skills
  would be **advertise-only** (ADR-0004 Phase 1) and therefore MUST NOT be
  attached to any Role — an advertised-but-unenforced privileged skill is a
  security regression. Gate the skill-CR delivery on this prerequisite.
- **Then (impl — delegated):** author the `org-ops` and `project-ops` Skill CRs
  (inline first, mirroring task-io's inline-while-API-settles pattern; convert to
  SHA-pinned catalog entries once the API schemas publish), and wire
  `defaultSkills[]` per the attachment gate in `examples/bmad-team`.

## Trust boundary (inherits ADR-0004 D8)

The skill body for org-ops/project-ops is untrusted behavior-data. The **only**
authorities are (a) `Skill.spec.permissions` for advertisement and (b) the
role-scoped coord token for enforcement — both authored/minted by the
operator/control-plane, never by anything the body says. A body that claims
broader scope than its role's token grants is simply denied at the API.

## Consequences

- The reference squad gains a coherent, board-operable capability set:
  contextasm (PUSH context) + task-io (own-task agency, all roles) + org-ops
  (CEO+managers) + project-ops (CEO). ICs stay least-privilege by construction.
- One new hard dependency: role-scoped token minting in the coord/apiserver
  layer. This is the gating item; the skill CRs are cheap once it lands.
- No new authorization subsystem: everything rides ADR-0004's projection seam
  and ISI-3601's coord-token seam.

## Rejected alternatives

- **org-ops/project-ops as one skill.** Rejected: project creation (CEO-only) and
  agent/skill creation (CEO+managers) have different gates; one skill would force
  the looser gate onto the more privileged verb.
- **Gate solely by `Role.spec.defaultSkills` attachment.** Rejected (D2): skills
  union with per-Agent `skillRefs`, so attachment is ergonomic, not a security
  boundary. Enforcement must live server-side on a role-derived token.
- **Advertise-only (ADR-0004 Phase 1) as the enforcement for these skills.**
  Rejected for privileged verbs: acceptable for self-scoped task-io, unsafe for
  org/project mutation. These skills wait for real server-side scope enforcement.
- **A bespoke runtime capability flag for org/project ops.** Rejected: forks a
  second authorization path parallel to skills for no benefit.
