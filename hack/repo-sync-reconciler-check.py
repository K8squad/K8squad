#!/usr/bin/env python3
"""Story 11.1 (ISI-2254) falsification — the per-`Project` repo-sync reconciler is a
LEVEL-TRIGGERED mirror behind a `SourceControlProvider` seam (GitHub v1), fed by an
HMAC-verified webhook FAST PATH and a periodic POLL FALLBACK, authorized by a per-user/
per-`Project` BYO Secret ref (never a shared platform token), producing an UNTRUSTED-
EXTERNAL, provenanced mirror that is NEVER the source of truth and NEVER writes coord
custody (arch §5.4 / ADR-018; Epic 9 webhook ingress; Epic 7 BYO creds; §6/§7.3.2 locks).

WHY THIS BENCH EXISTS
---------------------
CEO decision (Henrik, 2026-08-11): a `Project`'s upstream host (GitHub first) is
*mirrored*, not made the source of truth; the fenced coordination record (§6) stays
authoritative and custody never crosses the seam. Six ways a plausible-but-wrong
reconciler silently breaks the acceptance, none caught by "it compiles / it syncs once":

  1. SEAM-BLEED — the reconciler branches on the concrete provider (GitHub SDK types /
     `if provider == "github"`) instead of talking ONLY to the `SourceControlProvider`
     interface. GitLab/Gitea can no longer drop in behind the same seam (§10.2 spec-drift
     discipline); provider churn reaches coord. The reconciler MUST be provider-neutral.

  2. EDGE-TRIGGERED / NON-IDEMPOTENT — a webhook is treated as the state itself (its
     payload is written directly) instead of only *triggering* a level-triggered
     reconcile keyed by external id. Webhooks are lossy + at-least-once (§5.4): a partial
     payload leaves partial state, and a redelivered webhook double-applies. Reconcile
     MUST be an idempotent upsert; the webhook is only a fast path.

  3. NO-POLL-FALLBACK — sync is webhook-only. When webhooks are absent/dropped the mirror
     never converges (a missed delivery is permanent drift). A periodic provider-list
     reconcile — interval from values, not hardcoded — is the correctness backstop.

  4. VERIFY-AFTER-PARSE — the webhook payload is parsed / acted on BEFORE the HMAC
     signature is verified, or a bad/absent signature is accepted. An unsigned forgery
     then mutates mirror state (D8, NFR-SEC8). The signature MUST be verified before any
     payload is parsed; a bad-sig delivery is dropped, never reconciled.

  5. SHARED-TOKEN / LEAKED-TOKEN — the provider token is a shared platform/master token to
     GitHub, or the per-`Project` BYO token is logged / echoed / injected into an agent
     Run's env. Creds MUST come from the per-user/per-`Project` Secret ref (D3/FR-G1, same
     discipline as §11), scoped mirror-read, never logged, never exposed to a Run.

  6. MIRROR-AS-AUTHORITY — synced rows are treated as trusted control input (no
     `external_origin`, injected as authoritative context), the mirror writes coord
     custody (claim/lease/fence), or our own reflected writes re-enter as fresh inbound
     changes (no echo-suppression → ping-pong). The mirror is UNTRUSTED-EXTERNAL
     (§7.3.2), provenanced, single-writer on external-owned fields only; custody never
     crosses the seam (no-P2P/§6 stands).

One falsification layer, stdlib-only (`python3 repo-sync-reconciler-check.py`): a faithful
model of the repo-sync control loop — the provider seam, the webhook-trigger + poll-
fallback event loop, HMAC-before-parse, BYO token resolution, and the untrusted-external
provenanced mirror. Six checks C1–C6 ↔ AC1–AC6, GREEN on the §5.4-conformant baseline. A
battery of broken-reconciler mutations; EACH must flip its designated check RED. Several
checks are DIFFERENTIAL (reconcile the SAME records through two providers / two poll
intervals and assert the output tracks the seam/values, not a hardcode) — that is the
teeth against seam-bleed and hardcoding. There is no shipped Epic-11 Go artifact yet
(k8squad has only the API scaffold), so this pins the construction-time contract; the
runtime proof (the real `pkg/scm` seam, the apiserver HMAC webhook endpoint, the `scm`
schema upsert, and the BYO-Secret token resolution against a live GitHub) is owned by the
operator/apiserver integration tests as Epic 11 lands.

Exit non-zero if the baseline is not GREEN on all six or any mutation survives.
"""
import sys


# ══════════════════════════════════════════════════════════════════════════════════════
#  Project spec + provider seam. Two providers return the SAME normalized records so a
#  differential check proves the reconciler is provider-neutral (seam, not GitHub-coupled).
# ══════════════════════════════════════════════════════════════════════════════════════

def project(name="proj", repo="github.com/acme/app", interval=300):
    return {
        "name": name,
        "repo": repo,
        "sync": {
            "provider": "github",
            "tokenSecretRef": "acme-scm-token",       # per-Project BYO mirror-read Secret
            "webhookSecretRef": "acme-webhook-hmac",   # per-Project HMAC secret ref
            "pollIntervalSeconds": interval,           # from values, not hardcoded
        },
    }


# Normalized records the provider yields to the reconciler. Kind/external_id/state/actor is
# the common shape any SourceControlProvider maps its API onto — the reconciler sees ONLY
# this, never a GitHub-specific type. `ksquad-bot` is OUR own reflected write → must be
# echo-suppressed on the way back in (loop-prevention).
NORMALIZED = [
    {"kind": "pr",        "external_id": "1", "state": "open",    "title": "feat",  "actor": "dev"},
    {"kind": "issue",     "external_id": "7", "state": "open",    "title": "bug",   "actor": "dev"},
    {"kind": "check_run", "external_id": "3", "state": "success", "title": "ci",    "actor": "ci"},
    {"kind": "pr",        "external_id": "9", "state": "open",    "title": "echo",  "actor": "ksquad-bot"},
]
EXTERNAL_MIRRORED = [("check_run", "3"), ("issue", "7"), ("pr", "1")]   # bot record suppressed


class Provider:
    """A SourceControlProvider impl. `name` distinguishes GitHub (v1) from a drop-in
    (GitLab) — a provider-neutral reconciler produces identical mirror state from either."""

    def __init__(self, name):
        self.name = name

    def snapshot(self, mut=None):
        # Provider-specific fetch, normalized to the common shape. Identical across impls.
        return [dict(r) for r in NORMALIZED]

    def hmac_valid(self, sig, secret):
        return sig == "sig(%s)" % secret


def gh():
    return Provider("github")


def gl():
    return Provider("gitlab")


def good_sig(proj):
    return "sig(%s)" % proj["sync"]["webhookSecretRef"]


def resolve_token(proj, mut):
    if mut == "shared_master_token":
        # regression: one platform-wide master token to GitHub for every Project
        return {"value": "PLATFORM_MASTER", "shared": True, "scope": "admin", "ref": None}
    ref = proj["sync"]["tokenSecretRef"]
    return {"value": "secret:%s" % ref, "shared": False, "scope": "mirror-read", "ref": ref}


# ══════════════════════════════════════════════════════════════════════════════════════
#  The repo-sync control loop (model). Processes an event stream — ("wh", sig, ext_id)
#  webhook deliveries and ("poll",) periodic ticks — and returns the resulting mirror +
#  audit. `mut` injects one broken-reconciler bug.
# ══════════════════════════════════════════════════════════════════════════════════════

def run_reconcile(proj, provider, events, mut=None):
    st = {"mirror": {}, "logs": [], "run_env": {}, "dropped": [], "side_effects": [],
          "polls": 0, "scheduled_interval": None, "token": None}

    # ── reconciler startup: resolve the BYO provider token (never shared, never leaked) ──
    st["token"] = resolve_token(proj, mut)
    if mut == "log_token":
        st["logs"].append("scm auth token=%s" % st["token"]["value"])       # leak to logs
    if mut == "token_to_run_env":
        st["run_env"]["GIT_TOKEN"] = st["token"]["value"]                   # leak to a Run
    # poll cadence comes from values (differential target); hardcode freezes it
    st["scheduled_interval"] = 60 if mut == "hardcode_poll_interval" \
        else proj["sync"]["pollIntervalSeconds"]

    _pass = [0]

    def reconcile_from_provider():
        """Level-triggered: read the provider's current state and idempotent-upsert every
        record keyed by external id. Redelivery / re-poll of the same state is a no-op."""
        _pass[0] += 1
        if mut == "provider_type_branch" and provider.name != "github":
            return  # seam-bleed: only the concrete GitHub provider is handled
        for rec in provider.snapshot(mut):
            # loop-prevention: drop deliveries authored by our own reflected write
            if rec.get("actor") == "ksquad-bot" and mut != "no_echo_suppress":
                continue
            key = (rec["kind"], rec["external_id"])
            if mut == "non_idempotent":
                key = (rec["kind"], rec["external_id"], _pass[0])   # new row every pass
            row = {
                "kind": rec["kind"], "external_id": rec["external_id"],
                "state": rec["state"], "title": rec["title"],
                # external-owned fields only; provenance stamped external + untrusted
                "external_origin": None if mut == "drop_provenance" else {
                    "provider": provider.name, "repo": proj["repo"],
                    "external_id": rec["external_id"], "actor": rec["actor"]},
                "trust": "trusted-control" if mut == "trust_as_control" else "untrusted-external",
            }
            if mut == "mirror_writes_custody":
                row["claim"] = "held-by-mirror"    # custody through the mirror (regression)
            st["mirror"][key] = row

    for ev in events:
        if ev[0] == "wh":
            _, sig, ext = ev
            valid = provider.hmac_valid(sig, proj["sync"]["webhookSecretRef"])
            if mut == "accept_bad_sig":
                valid = True
            if mut == "parse_before_verify":
                st["side_effects"].append(ext)     # parsed/acted BEFORE the verify gate
            if not valid:
                st["dropped"].append(ext)
                continue
            if mut == "edge_triggered":
                # regression: write the webhook payload directly, no provider reconcile
                st["mirror"][("pr", ext)] = {
                    "kind": "pr", "external_id": ext, "state": "?", "title": "?",
                    "external_origin": {"provider": provider.name, "repo": proj["repo"],
                                        "external_id": ext, "actor": "?"},
                    "trust": "untrusted-external"}
            else:
                reconcile_from_provider()          # webhook only TRIGGERS a reconcile
        elif ev[0] == "poll":
            st["polls"] += 1
            if mut == "webhook_only":
                continue                           # no poll fallback → drift on missed hook
            reconcile_from_provider()
    return st


def norm(st):
    return sorted((k[0], k[1]) for k in st["mirror"])


# ══════════════════════════════════════════════════════════════════════════════════════
#  Checks C1–C6 ↔ AC1–AC6. Each takes the mutation under test (None = conformant baseline)
#  and returns True on a conformant reconciler, False on a broken one.
# ══════════════════════════════════════════════════════════════════════════════════════

def c1_provider_seam(mut):
    """AC1 — the reconciler talks ONLY to the SourceControlProvider seam: GitHub and a
    drop-in (GitLab) yield IDENTICAL mirror state from identical records (differential)."""
    p = project()
    mg = run_reconcile(p, gh(), [("poll",)], mut)
    ml = run_reconcile(p, gl(), [("poll",)], mut)
    return norm(mg) == EXTERNAL_MIRRORED and norm(ml) == EXTERNAL_MIRRORED


def c2_webhook_triggers_idempotent(mut):
    """AC2 — a webhook TRIGGERS a full level-triggered reconcile (not just its own
    payload), and a redelivered webhook is an idempotent no-op (one row per external id)."""
    p = project()
    g = good_sig(p)
    st = run_reconcile(p, gh(), [("wh", g, "1"), ("wh", g, "1")], mut)
    full = norm(st) == EXTERNAL_MIRRORED                       # whole provider state, not just PR 1
    idempotent = len([k for k in st["mirror"] if k[:2] == ("pr", "1")]) == 1
    return full and idempotent


def c3_poll_fallback(mut):
    """AC3 — with NO webhooks the periodic poll converges the mirror, and the poll interval
    tracks values (300 vs 900), never a hardcode (differential)."""
    conv = run_reconcile(project(), gh(), [("poll",)], mut)
    converges = norm(conv) == EXTERNAL_MIRRORED
    ia = run_reconcile(project(interval=300), gh(), [], mut)["scheduled_interval"]
    ib = run_reconcile(project(interval=900), gh(), [], mut)["scheduled_interval"]
    from_values = (ia == 300 and ib == 900)
    return converges and from_values


def c4_hmac_before_parse(mut):
    """AC4 — a bad/absent HMAC signature is dropped BEFORE any parse: no mirror write, no
    side effect; a good signature reconciles."""
    p = project()
    bad = run_reconcile(p, gh(), [("wh", "sig(FORGED)", "1")], mut)
    dropped = ("1" in bad["dropped"]) and not bad["mirror"] and not bad["side_effects"]
    good = bool(run_reconcile(p, gh(), [("wh", good_sig(p), "1")], mut)["mirror"])
    return dropped and good


def c5_byo_creds(mut):
    """AC5 — the provider token is a per-Project BYO Secret ref, scoped mirror-read, never
    a shared master, never logged, never injected into a Run's env."""
    p = project()
    st = run_reconcile(p, gh(), [("poll",)], mut)
    tok = st["token"]
    byo = (not tok["shared"]) and tok["ref"] == p["sync"]["tokenSecretRef"] \
        and tok["scope"] == "mirror-read"
    not_logged = all(tok["value"] not in line for line in st["logs"])
    not_in_run = tok["value"] not in st["run_env"].values()
    return byo and not_logged and not_in_run


def c6_mirror_untrusted_not_authority(mut):
    """AC6 — every mirrored row is UNTRUSTED-EXTERNAL + provenanced (external_origin), the
    mirror writes NO coord custody, and our own reflected write is echo-suppressed."""
    st = run_reconcile(project(), gh(), [("poll",)], mut)
    rows = list(st["mirror"].values())
    all_untrusted = all(r["trust"] == "untrusted-external" for r in rows)
    all_provenanced = all(r["external_origin"] for r in rows)
    no_custody = all("claim" not in r for r in rows)
    echo_suppressed = all(r["external_id"] != "9" for r in rows)
    return all_untrusted and all_provenanced and no_custody and echo_suppressed and len(rows) >= 3


CHECKS = {
    "C1": ("AC1 provider seam (GitHub/GitLab neutral)", c1_provider_seam),
    "C2": ("AC2 webhook triggers idempotent reconcile", c2_webhook_triggers_idempotent),
    "C3": ("AC3 poll fallback + interval from values",  c3_poll_fallback),
    "C4": ("AC4 HMAC verified before parse",            c4_hmac_before_parse),
    "C5": ("AC5 BYO per-Project token, never leaked",   c5_byo_creds),
    "C6": ("AC6 mirror untrusted-external, not authority", c6_mirror_untrusted_not_authority),
}

# Mutation → the check that must catch it.
MUTATIONS = [
    ("M1  branch on concrete provider (seam bleed)",   "provider_type_branch", "C1"),
    ("M2  webhook payload IS the state (edge-trig)",   "edge_triggered",       "C2"),
    ("M3  reconcile not idempotent (dup per hook)",    "non_idempotent",       "C2"),
    ("M4  webhook-only, no poll fallback",             "webhook_only",         "C3"),
    ("M5  hardcode poll interval (ignore values)",     "hardcode_poll_interval","C3"),
    ("M6  accept bad HMAC signature",                  "accept_bad_sig",       "C4"),
    ("M7  parse/act before verifying HMAC",            "parse_before_verify",  "C4"),
    ("M8  shared platform/master token",               "shared_master_token",  "C5"),
    ("M9  log the provider token",                     "log_token",            "C5"),
    ("M10 inject token into a Run's env",              "token_to_run_env",     "C5"),
    ("M11 mirror row as trusted control input",        "trust_as_control",     "C6"),
    ("M12 drop external_origin provenance",            "drop_provenance",      "C6"),
    ("M13 mirror writes coord custody (claim)",        "mirror_writes_custody","C6"),
    ("M14 no echo-suppression (ping-pong)",            "no_echo_suppress",     "C6"),
]


def main():
    print("=" * 92)
    print("Story 11.1 (ISI-2254) — repo-sync reconciler falsification. Model of the §5.4 control")
    print("loop: provider seam + webhook-trigger/poll-fallback + HMAC-before-parse + BYO creds +")
    print("untrusted-external provenanced mirror. Baseline GREEN on C1–C6; every mutation caught.")
    print("=" * 92)
    ok_all = True

    print("\n  Baseline (§5.4 / ADR-018-conformant reconciler):")
    for cid, (label, fn) in CHECKS.items():
        ok = fn(None)
        ok_all &= ok
        print(f"    [{'GREEN' if ok else 'RED  '}] {cid} {label}")

    print("\n  Mutation battery (each must flip its designated check RED):")
    for mlabel, mut, cid in MUTATIONS:
        _, fn = CHECKS[cid]
        caught = not fn(mut)
        ok_all &= caught
        print(f"    [{'CAUGHT ' if caught else 'SURVIVED'}] {mlabel:<40} → {cid} "
              f"{'RED' if caught else 'still GREEN'}")

    print("\n" + "=" * 92)
    if ok_all:
        print("✓ ALL GREEN — baseline passes C1–C6; all 14 mutations caught, no vacuous survivors. "
              "Story 11.1 acceptance is falsifiable.")
        return 0
    print("✗ FAILURES ABOVE — see RED / SURVIVED rows.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
