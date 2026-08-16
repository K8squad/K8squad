#!/usr/bin/env python3
"""Story X.1 (ISI-2240) falsification — hostile-Run blast-radius containment (arch §4.3, §12, §17.1,
S4, NFR-SEC1). Required CI artifact.

┌─ SCOPE: DESIGN-LEVEL MODEL GATE (read this before trusting a green run) ─────────────────────────┐
│ This gate falsifies the isolation *model*, NOT the running cluster. `posture_contained()` and     │
│ `posture_porous()` below are HAND-AUTHORED in-file dicts, not parsed from the repo's manifests.    │
│ Consequence (retro-review ISI-2537 F2): a real regression that ships in `config/` — e.g. a         │
│ ClusterRole granting `secrets: list`, or a workspace PVC mounted at root — does NOT flip this gate │
│ red, because this gate never reads `config/`. A green run here means "the modelled arch posture    │
│ contains the modelled adversary and the porous foil is detectably breached" — it is an always-on   │
│ guard against the *design* silently regressing, not proof of runtime containment.                  │
│                                                                                                    │
│ Two follow-ups close that gap (tracked on the L4-security lane, ISI-2245):                          │
│   1. ANCHOR: once k8squad `config/` carries real per-team NetworkPolicy + RBAC manifests, parse    │
│      them into `posture_contained()` so this gate fails on an actual manifest regression.          │
│   2. RUNTIME: the kind-based S4 e2e (real NetworkPolicy/RBAC in a live cluster, testing-strategy    │
│      §6.5 integration layer) is the runtime counterpart this model gate stands in for until it lands.│
└────────────────────────────────────────────────────────────────────────────────────────────────┘

Threat model (arch §17.1): every data-plane principal is HOSTILE. A Run executes arbitrary LLM-authored
shell/git/build. This harness proves that a hostile Run in Team-A (principal `alice`) trying to reach
ANOTHER squad's / another principal's / a prior Run's **network, secrets, or workspace** is CONTAINED —
the attempt fails — while the legitimate paths a Run needs (DNS, control plane, its own allowlisted
model endpoint, its own Secret, its own worktree) still work.

Why *computed*, not asserted (§17.1 "tested, not asserted", F6/F7). Containment is not eyeballed: the
harness carries a small **NetworkPolicy evaluator** (K8s egress semantics), an **RBAC/Secret evaluator**
(namespaced Role + `resourceNames` get-by-name), a **workspace-scoping evaluator** (per-principal PVC
subPath), and a **teardown-residue evaluator** (§9.3 teardown-and-replace). Each hostile attempt's
outcome is DERIVED from the posture, so a weakened posture flips the verdict.

Why differential (real teeth). A happy-path "the pod couldn't reach evil.example once" demo passes on a
posture that leaks three other ways. We model TWO postures:
  - `posture_contained`  — the arch §9/§12/§17.1 posture (default-deny egress + named/audited model hole,
                           namespaced least-priv RBAC get-by-name, per-principal PVC subPath,
                           teardown-and-replace). Must hold EVERY blast-radius invariant.
  - `posture_porous`     — a plausible-but-wrong posture (no egress policy, ClusterRole cross-ns secrets,
                           shared PVC root mount, in-place reset leaving residue). Every containment axis
                           this story pins is BREACHED — the harness must DETECT each, or it has no teeth.

Gate ISI-2113 (RuntimeClass pick) — CLEARED: gVisor default / Kata opt-in / runc rejected for untrusted
code (Story 4.2, ISI-2292/2294). This harness is the S4 blast-radius layer that sits ON TOP of that
kernel boundary (namespace + NetworkPolicy + RBAC + per-principal workspace).

stdlib only; `python3 hostile-run-blast-radius-check.py` (exit 0 = contained + teeth intact).
"""
import sys

SYSTEM_NS = "ksquad-system"
NS_A = "ksquad-team-alpha-3f1aaaaa"   # attacker's squad
NS_B = "ksquad-team-bravo-9c2bbbbb"   # victim squad
KUBE_SYSTEM = "kube-system"           # kube-dns lives here


# ---- world: the fixed entities every posture shares ----------------------------------------------

# The hostile Run: Team-A, principal alice, its own SA / Secret / workspace subpath.
ATTACKER = {"ns": NS_A, "labels": {"app": "ksquad-run"}, "principal": "alice",
            "sa": "ksquad-agent", "own_secret": f"ksquad-cred-alice.{NS_A}",
            "workspace_pvc": "proj-x-workspace"}

# Targets the hostile Run will try to reach. `class` drives the NetworkPolicy match; `expect` is the
# security verdict the arch posture must produce ("deny" = contained, "allow" = a legitimate path).
NET_TARGETS = [
    # cross-squad network — Team-B service (NFR-SEC1): a Team-A Run must NOT reach it.
    {"id": "team-b-svc", "cls": "team-ns", "ns": NS_B, "port": 8080, "expect": "deny"},
    # arbitrary internet exfil endpoint (NFR-SEC4, default-deny egress): denied.
    {"id": "evil.example", "cls": "internet", "host": "evil.example", "port": 443, "expect": "deny"},
    # the allowlisted model endpoint (F11 named hole): ALLOWED — but must be AUDITED, not silent.
    {"id": "api.model.example", "cls": "model", "host": "api.model.example", "port": 443,
     "expect": "allow", "must_audit": True},
    # DNS + control plane: legitimate egress the Run needs to function.
    {"id": "kube-dns", "cls": "dns", "ns": KUBE_SYSTEM, "port": 53, "expect": "allow"},
    {"id": "apiserver", "cls": "control-plane", "ns": SYSTEM_NS, "port": 443, "expect": "allow"},
]

# Secrets the hostile Run will try to read.
SECRET_TARGETS = [
    {"id": "bravo-cred",  "name": f"ksquad-cred-carol.{NS_B}", "ns": NS_B, "expect": "deny"},  # cross-squad
    {"id": "peer-cred",   "name": f"ksquad-cred-bob.{NS_A}",   "ns": NS_A, "expect": "deny"},  # same-ns peer
    {"id": "own-cred",    "name": f"ksquad-cred-alice.{NS_A}", "ns": NS_A, "expect": "allow"}, # own (needed)
]

# Workspace paths the hostile Run will try to read on the shared Project PVC.
WS_TARGETS = [
    {"id": "peer-cache", "path": "principal/bob/cache",   "principal": "bob",   "expect": "deny"},
    {"id": "own-cache",  "path": "principal/alice/cache", "principal": "alice", "expect": "allow"},
]


# ---- postures: each returns the security controls the platform would install ----------------------

def posture_contained():
    """arch §9/§12/§17.1: default-deny egress + audited model hole; namespaced get-by-name RBAC;
    per-principal PVC subPath; teardown-and-replace (fresh sandbox, zero residue)."""
    return {
        # NetworkPolicies in NS_A selecting the Run pod for Egress. Any egress policy selecting the pod
        # makes unlisted egress DENIED (K8s semantics) — so the allow-set below is exhaustive.
        "netpol": [
            {"ns": NS_A, "policyTypes": ["Ingress", "Egress"], "podSelector": {},
             "egress": []},                                              # default-deny
            {"ns": NS_A, "policyTypes": ["Egress"], "podSelector": {},
             "egress": [{"allow_cls": "dns", "ports": [53]}]},
            {"ns": NS_A, "policyTypes": ["Egress"], "podSelector": {},
             "egress": [{"allow_cls": "control-plane", "ports": [443]}]},
            {"ns": NS_A, "policyTypes": ["Egress"], "podSelector": {},
             "egress": [{"allow_cls": "model", "ports": [443], "audited": True}]},  # F11 named hole
        ],
        # RBAC: a NAMESPACED Role granting secrets get-by-name for alice's own cred only.
        "rbac": [
            {"scope": "namespaced", "ns": NS_A, "sa": "ksquad-agent", "resources": ["secrets"],
             "verbs": ["get"], "resourceNames": [f"ksquad-cred-alice.{NS_A}"]},
        ],
        # Workspace: the Run mounts the Project PVC scoped to alice's per-principal subPath (§9.4).
        "workspace_mount": {"pvc": "proj-x-workspace", "subPath": "principal/alice"},
        # Hygiene: teardown-and-replace — the fresh sandbox carries NO prior-Run residue (§9.3).
        "residue": [],
    }


def posture_porous():
    """Plausible-but-wrong: NO egress policy (all egress open), a ClusterRole granting cross-namespace
    `secrets: list`, the PVC mounted at ROOT (all principals visible), and in-place reset that leaves a
    prior Run's scratch/worktree/secret behind. Every blast-radius axis is breached."""
    return {
        "netpol": [],  # no egress policy at all → K8s allows ALL egress (internet + Team-B reachable)
        "rbac": [
            {"scope": "cluster", "ns": None, "sa": "ksquad-agent", "resources": ["secrets"],
             "verbs": ["get", "list"], "resourceNames": []},  # cluster-wide, no name scoping
        ],
        "workspace_mount": {"pvc": "proj-x-workspace", "subPath": ""},  # ROOT: every principal visible
        # in-place reset residue: prior Run's leftovers survive into the "fresh" sandbox
        "residue": ["principal/bob/cache/creds.env", "worktree/.git/config", "/tmp/run-prev/scratch"],
    }


# ---- evaluators: derive each hostile attempt's outcome from the posture ---------------------------

def egress_reachable(target, netpols):
    """K8s Egress semantics: if NO egress policy selects the pod, ALL egress is allowed. If ≥1 does,
    only traffic matching some egress rule is allowed. Returns (reachable, audited)."""
    egress_policies = [p for p in netpols if "Egress" in p["policyTypes"]]  # podSelector {} = all pods
    if not egress_policies:
        return True, False  # nothing constrains egress → wide open (the porous breach)
    for p in egress_policies:
        for rule in p.get("egress", []):
            if rule.get("allow_cls") == target["cls"] and target.get("port") in rule.get("ports", []):
                return True, bool(rule.get("audited"))
    return False, False


def secret_readable(secret, rbac, attacker):
    """A NAMESPACED Role can only grant within its own namespace; a ClusterRole reaches every namespace.
    `get` is scoped to `resourceNames`; `list`/`watch` (or `get` without resourceNames) exposes every
    Secret in reach — defeating per-principal isolation (§12.1)."""
    for g in rbac:
        if "secrets" not in g["resources"]:
            continue
        # namespace reach: cluster scope reaches everywhere; namespaced Role only its own ns.
        if g["scope"] == "namespaced" and g["ns"] != secret["ns"]:
            continue
        wide = bool(set(g["verbs"]) & {"list", "watch"}) or (
            "get" in g["verbs"] and not g.get("resourceNames"))
        if wide:
            return True  # namespace-wide read reaches this secret
        if "get" in g["verbs"] and secret["name"] in g.get("resourceNames", []):
            return True  # get-by-name, and this is a named secret (only alice's own)
    return False


def workspace_readable(target, mount):
    """The Run only sees paths UNDER its mounted subPath. subPath '' = PVC root = every principal."""
    sub = mount["subPath"]
    if sub == "":
        return True  # root mount: all principals' subtrees visible (the porous breach)
    return target["path"].startswith(sub.rstrip("/") + "/")


# ---- the differential harness --------------------------------------------------------------------

def evaluate(posture):
    """Run every hostile attempt against `posture`; return (breaches, functional_denials)."""
    breaches, funcdenials = [], []
    p = posture()

    # --- network axis ---
    for t in NET_TARGETS:
        reachable, audited = egress_reachable(t, p["netpol"])
        if t["expect"] == "deny" and reachable:
            breaches.append(f"NET  hostile Run reached {t['id']} ({t['cls']}) — cross-squad/exfil egress "
                            f"not contained (NFR-SEC1/SEC4, §12.2)")
        if t["expect"] == "allow":
            if not reachable:
                funcdenials.append(f"NET  legitimate egress to {t['id']} blocked — Run non-functional")
            elif t.get("must_audit") and not audited:
                breaches.append(f"NET  {t['id']} allowlisted hole is SILENT — the named egress hole must "
                                f"be audited (egress proxy), not mistaken for containment (review F11)")

    # --- secret axis ---
    for t in SECRET_TARGETS:
        readable = secret_readable(t, p["rbac"], ATTACKER)
        if t["expect"] == "deny" and readable:
            breaches.append(f"SEC  hostile Run read {t['id']} ({t['name']}) — cross-squad/cross-principal "
                            f"Secret not contained (NFR-SEC1, §12.1 per-principal isolation)")
        if t["expect"] == "allow" and not readable:
            funcdenials.append(f"SEC  Run cannot read its OWN Secret {t['name']} — credential injection broken")

    # --- workspace axis ---
    for t in WS_TARGETS:
        readable = workspace_readable(t, p["workspace_mount"])
        if t["expect"] == "deny" and readable:
            breaches.append(f"WS   hostile Run read {t['id']} ({t['path']}) — another principal's workspace "
                            f"leaked on the shared Project PVC (NFR-SEC5, §9.4 per-principal subPath)")
        if t["expect"] == "allow" and not readable:
            funcdenials.append(f"WS   Run cannot read its OWN workspace {t['path']} — worktree broken")

    # --- reuse-residue axis (§9.3 teardown-and-replace, F6/F7) ---
    if p["residue"]:
        breaches.append(f"RES  fresh sandbox exposes {len(p['residue'])} prior-Run artifact(s) "
                        f"{p['residue']} — teardown-and-replace not honored; reuse-residue leak (§9.3, F6/F7)")

    return breaches, funcdenials


def main():
    # 1) Teeth: the porous posture must be DETECTED as breached on every axis.
    porous_breaches, _ = evaluate(posture_porous)
    axes = {b.split()[0] for b in porous_breaches}
    print(f"[model] porous posture : {len(porous_breaches)} containment breach(es) across axes "
          f"{sorted(axes)} -> {'DETECTED' if porous_breaches else 'NONE (teeth lost!)'}")
    for b in porous_breaches:
        print(f"[model]   BREACH {b}")
    required = {"NET", "SEC", "WS", "RES"}
    if not required.issubset(axes):
        print(f"[model] FAIL — porous posture must breach every axis {sorted(required)}; "
              f"missing {sorted(required - axes)}. Harness has no teeth on those axes.")
        return 1

    # 2) The arch posture must CONTAIN every hostile attempt AND keep legitimate paths working.
    good_breaches, good_denials = evaluate(posture_contained)
    print(f"[model] arch posture   : {len(good_breaches)} containment breach(es), "
          f"{len(good_denials)} functional denial(s)")
    for b in good_breaches:
        print(f"[model]   BREACH {b}")
    for d in good_denials:
        print(f"[model]   DENIED {d}")
    if good_breaches:
        print("[model] FAIL — the arch §9/§12/§17.1 posture must contain every hostile attempt (S4).")
        return 1
    if good_denials:
        print("[model] FAIL — containment must not break the legitimate paths a Run needs (DNS/CP/model/"
              "own-Secret/own-worktree).")
        return 1

    print("[model] PASS — hostile Run contained on network+secret+workspace+reuse-residue; "
          "porous posture detectably breaches each; legitimate paths intact (S4/NFR-SEC1).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
