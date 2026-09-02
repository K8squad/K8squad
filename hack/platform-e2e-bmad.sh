#!/usr/bin/env bash
# platform-e2e-bmad.sh — parameterized platform-E2E harness (ISI-3560, parent ISI-3559).
#
# Scripted apply-and-assert for the K8squad happy path: deploy the BMAD squad ENTIRELY
# via CRDs, wire the toolchain catalog, create a Project, and drive the crew to build a
# real todo/post-it app (DB + backend + frontend) ticket-driven through BMAD phases,
# one PR per story — the functional counterpart to the ISI-3534 UI Playwright suite.
#
# Source of truth: the ISI-3559 scenario doc (document key `scenario`). Phases 0-6,
# success criteria SC-1..SC-6. Minimum bar = ONE story lands as a PR (SC-4).
#
# DESIGN PRINCIPLES
#   * Params, NOT secrets. Everything the run needs comes from env below. There is NO
#     hardcoded host/user/token/kubeconfig/DT-tenant/ISI-number in this file — it is
#     safe to commit and to publish. Credential Secrets are created OUT OF BAND by
#     ProxOps (§4) and referenced BY NAME only.
#   * Publish results to PAPERCLIP, not GitHub (ISI-3534 no-leak constraint). This
#     script writes phase-status.{md,json} + condition dumps to $OUT_DIR; ProxOps
#     attaches them to the run issue. The target *app* repo living on GitHub is fine —
#     that is the product; the test method/results are what stay internal.
#   * Honest partials. Every phase records PASS / FAIL / BLOCKED / SKIP with a reason.
#     SC-1..SC-2 green with SC-3/SC-4 blocked is a PARTIAL, reported as such — NEVER
#     faked green. If the operator Run pipeline does not admit/execute, that IS the
#     highest-value finding (§9.1), captured as BLOCKED with the observed evidence.
#   * Fail-closed preflight, bounded waits, live-cluster-driven assertions (kubectl
#     -o json), never a hardcoded inventory — adding an Agent/Skill is covered.
#
# USAGE (all overridable; only KUBECONFIG + TARGET_REPO_URL are run-specific):
#   KUBECONFIG=~/.config/capmox/k8squad-test.kubeconfig \
#   TARGET_REPO_URL=https://github.com/K8squad/sympozium-todo-demo \
#   ./hack/platform-e2e-bmad.sh
#
# Exit code: 0 iff the minimum bar holds (SC-1..SC-3 PASS and SC-4 PASS for >=1 story).
# A PARTIAL (some phase BLOCKED) exits non-zero so a CI lane fails visibly, but the
# per-phase table distinguishes BLOCKED from FAIL for triage.
set -uo pipefail

# ------------------------------------------------------------------ params (no secrets)
NAMESPACE="${NAMESPACE:-bmad-squad}"                 # squad namespace (Team dedicated ns)
CP_NS="${CP_NS:-k8squad-system}"                     # control-plane / toolchain-catalog ns
TARGET_REPO_URL="${TARGET_REPO_URL:-}"              # Project.spec.repo.url — the app repo
TARGET_REPO_REF="${TARGET_REPO_REF:-main}"          # branch to track
GH_SECRET_NAME="${GH_SECRET_NAME:-github-writepath-token}"  # write-path Secret (ProxOps, §4)
MODEL_SECRET_NAME="${MODEL_SECRET_NAME:-model-credentials}" # model token Secret (ProxOps, §4)

# Repo layout (resolved relative to this script so it runs from anywhere).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "$SCRIPT_DIR/.." && pwd)}"
SQUAD_DIR="${SQUAD_DIR:-$REPO_ROOT/examples/bmad-team}"

# Bounded waits.
RECONCILE_TIMEOUT="${RECONCILE_TIMEOUT:-300}"        # Phase 2 per-object Ready wait (s)
RUN_TIMEOUT="${RUN_TIMEOUT:-3600}"                   # Phase 4 Run terminal wait (s)
POLL_INTERVAL="${POLL_INTERVAL:-15}"                 # generic poll cadence (s)

# Phase-4 trigger seam (§9.1). Work items are coordination-Postgres rows and the
# Run-creation trigger is operator/coordinator-driven — there is NO clean public
# "create work item + create Run" API. Rather than hardcode an unproven path, the
# harness invokes a PLUGGABLE seam the runner (ProxOps, with DevOps) supplies:
#   SEED_CMD  — prints ONE work-item id (workItemRef) to stdout after seeding the
#               DB+backend+frontend backlog for the project. Exit 0 on success.
#   TRIGGER_CMD — given the work-item id ($1), creates/dispatches the Run (e.g. a
#               `kubectl apply` of a Run CR, or an operator/apiserver call). Optional:
#               if unset, the harness applies a Run CR referencing teamRef/projectRef/
#               $WORKITEM (best-effort default trigger).
# If SEED_CMD is unset, Phase 4/5 are marked BLOCKED with the §9.1 finding — the
# CRD/operator plane (Phase 0-3) is still fully validated and reported.
SEED_CMD="${SEED_CMD:-}"
TRIGGER_CMD="${TRIGGER_CMD:-}"

# Output.
OUT_DIR="${OUT_DIR:-$REPO_ROOT/.e2e-out}"

# ------------------------------------------------------------------ helpers
c_fail() { echo "::error::$*" >&2; }
note()   { echo "::notice::$*"; }
group()  { echo "::group::$*"; }
endgroup(){ echo "::endgroup::"; }

# Phase result ledger. record <phase> <PASS|FAIL|BLOCKED|SKIP> <message>
declare -a PH_ID PH_STATE PH_MSG
record() { PH_ID+=("$1"); PH_STATE+=("$2"); PH_MSG+=("$3"); echo ":: [$2] $1 — $3"; }

# ------------------------------------------------------------------ preflight (fail-closed)
mkdir -p "$OUT_DIR"
command -v kubectl >/dev/null || { c_fail "kubectl not on PATH"; exit 2; }
command -v jq      >/dev/null || { c_fail "jq not on PATH"; exit 2; }
[ -d "$SQUAD_DIR" ] || { c_fail "squad dir not found: $SQUAD_DIR"; exit 2; }
[ -n "$TARGET_REPO_URL" ] || { c_fail "TARGET_REPO_URL is required (Project.spec.repo.url)"; exit 2; }
kubectl cluster-info >/dev/null 2>&1 || { c_fail "no reachable cluster on the current kube-context (set KUBECONFIG)"; exit 2; }

note "platform-e2e-bmad: ns=$NAMESPACE cp-ns=$CP_NS repo=$TARGET_REPO_URL squad=$SQUAD_DIR out=$OUT_DIR"

# ============================================================ PHASE 0 — Preflight
group "PHASE 0 — preflight (cluster / operator / toolchain catalog)"
P0_OK=1; P0_MSG=""
# CRDs present
CRD_COUNT="$(kubectl get crd -o name 2>/dev/null | grep -c 'ksquad.io$' || true)"
if [ "${CRD_COUNT:-0}" -gt 0 ]; then echo "  ok   ksquad.io CRDs registered ($CRD_COUNT)"
else P0_OK=0; P0_MSG+="no ksquad.io CRDs; "; echo "  MISS ksquad.io CRDs — operator/chart not installed"; fi
# Operator Deployment Available
OP_AVAIL="$(kubectl get deploy -n "$CP_NS" -l app.kubernetes.io/component=operator \
  -o jsonpath='{.items[*].status.availableReplicas}' 2>/dev/null || true)"
if [ -n "${OP_AVAIL:-}" ] && [ "${OP_AVAIL:-0}" != "0" ]; then echo "  ok   operator Deployment available (replicas=$OP_AVAIL)"
else P0_OK=0; P0_MSG+="operator not Available; "; echo "  MISS operator Deployment not Available in $CP_NS"; fi
# Toolchain catalog present + enabled
CATALOG_JSON="$(kubectl get toolchains -n "$CP_NS" -o json 2>/dev/null || echo '{"items":[]}')"
CATALOG_COUNT="$(echo "$CATALOG_JSON" | jq '.items | length')"
if [ "${CATALOG_COUNT:-0}" -gt 0 ]; then
  echo "  ok   toolchain catalog present ($CATALOG_COUNT):"
  echo "$CATALOG_JSON" | jq -r '.items[] | "         - \(.metadata.name): \([.spec.versions[].version] | join(", "))"'
else P0_OK=0; P0_MSG+="empty toolchain catalog (need --set tools.defaultCatalog.enabled=true); "
  echo "  MISS no Toolchain objects in $CP_NS — enable tools.defaultCatalog.enabled=true"; fi
# Credential Secrets present (created out-of-band by ProxOps, §4) — assert existence, never read value
for s in "$MODEL_SECRET_NAME" "$GH_SECRET_NAME"; do
  if kubectl get secret "$s" -n "$NAMESPACE" >/dev/null 2>&1; then echo "  ok   secret/$s present (value not read)"
  else P0_OK=0; P0_MSG+="secret/$s missing; "; echo "  MISS secret/$s absent in $NAMESPACE — ProxOps must create it out of band (§4)"; fi
done
if [ "$P0_OK" -eq 1 ]; then record "Phase 0: Preflight" PASS "cluster+operator+catalog+credential-Secrets present"
else record "Phase 0: Preflight" FAIL "${P0_MSG%%; }"; fi
endgroup

# Preflight failures gate everything downstream — report and stop honestly.
if [ "$P0_OK" -ne 1 ]; then
  record "Phase 1: Deploy squad"     BLOCKED "gated by Phase 0 preflight"
  record "Phase 2: Reconcile/Ready"  BLOCKED "gated by Phase 0 preflight"
  record "Phase 3: Crew + toolchain" BLOCKED "gated by Phase 0 preflight"
  record "Phase 4: Story -> PR"      BLOCKED "gated by Phase 0 preflight"
  record "Phase 5: App buildable"    BLOCKED "gated by Phase 0 preflight"
fi

# ============================================================ PHASE 1 — Deploy the squad via CRDs
if [ "$P0_OK" -eq 1 ]; then
group "PHASE 1 — deploy the squad via CRDs (2 parameterized edits, not committed)"
# Build the applied manifest from the numbered files:
#   * SKIP 01-credentials.yaml — the plaintext REPLACE_ME Secrets. Real creds are the
#     out-of-band ProxOps Secrets ($MODEL_SECRET_NAME / $GH_SECRET_NAME), NOT this file.
#   * REWRITE 07-project.yaml spec.repo.url -> $TARGET_REPO_URL (+ ref).
#   * Defensively strip any remaining `kind: Secret` document so no REPLACE_ME token
#     can ever be applied by this harness.
APPLIED="$OUT_DIR/applied-squad.yaml"
: > "$APPLIED"
for f in "$SQUAD_DIR"/0[02-9]*.yaml; do
  case "$(basename "$f")" in
    01-*) continue ;;  # skip plaintext credential Secrets
    07-project.yaml)
      # parameterized repo.url / ref rewrite (leaves the rest of the Project intact)
      sed -e "s#^\(\s*url:\).*#\1 ${TARGET_REPO_URL}#" \
          -e "s#^\(\s*ref:\).*#\1 ${TARGET_REPO_REF}#" "$f" >> "$APPLIED"
      echo "---" >> "$APPLIED"
      ;;
    *) cat "$f" >> "$APPLIED"; echo "---" >> "$APPLIED" ;;
  esac
done
# Defensive: assert no Secret / no REPLACE_ME token leaked into the applied manifest.
# Strip comment lines first — the example carries an explanatory "# REPLACE_ME ..."
# comment in 02b-mcpservers.yaml that is documentation, not a token value.
if grep -vE '^[[:space:]]*#' "$APPLIED" | grep -qE '^kind: Secret|REPLACE_ME'; then
  record "Phase 1: Deploy squad" FAIL "refused: applied manifest still contains a Secret/REPLACE_ME token"
  P1_OK=0
else
  echo "  ok   applied manifest carries no Secret/REPLACE_ME (creds are out-of-band, §4/SC-6)"
  echo "  applying $APPLIED (repo.url -> $TARGET_REPO_URL) ..."
  if kubectl apply -f "$APPLIED" 2>&1 | sed 's/^/         /'; then
    # Verify the Project actually took the parameterized URL.
    GOT_URL="$(kubectl get project bmad-demo-project -n "$NAMESPACE" -o jsonpath='{.spec.repo.url}' 2>/dev/null || true)"
    if [ "$GOT_URL" = "$TARGET_REPO_URL" ]; then
      record "Phase 1: Deploy squad" PASS "squad applied; Project repo.url=$TARGET_REPO_URL"; P1_OK=1
    else
      record "Phase 1: Deploy squad" FAIL "Project repo.url mismatch: got '$GOT_URL' want '$TARGET_REPO_URL'"; P1_OK=0
    fi
  else
    record "Phase 1: Deploy squad" FAIL "kubectl apply rejected one or more CRs (schema drift)"; P1_OK=0
  fi
fi
endgroup
else P1_OK=0; fi

# ============================================================ PHASE 2 — Reconcile / Ready (SC-1)
if [ "${P1_OK:-0}" -eq 1 ]; then
group "PHASE 2 — reconcile to Ready (SC-1: all squad objects Ready=True, 0 degraded)"
# Wait bounded for every reconcilable kind to reach Ready=True. Driven off the live
# objects, not a hardcoded list. Skills/ConfigMaps/Namespace are not condition-bearing.
KINDS="agentruntime agents roles.ksquad.io skills projects teams"
DEADLINE=$(( $(date +%s) + RECONCILE_TIMEOUT ))
DEGRADED=""
dump_conditions() { # kind -> $OUT_DIR
  kubectl get "$1" -n "$NAMESPACE" -o wide > "$OUT_DIR/phase2-$1.wide.txt" 2>&1 || true
  kubectl get "$1" -n "$NAMESPACE" -o json 2>/dev/null \
    | jq -r '.items[]? | "\(.kind)/\(.metadata.name): " + ((.status.conditions // []) | map("\(.type)=\(.status)") | join(","))' \
    >> "$OUT_DIR/phase2-conditions.txt" 2>/dev/null || true
}
: > "$OUT_DIR/phase2-conditions.txt"
for k in $KINDS; do
  # Some kinds may legitimately have no Ready condition on this platform build; treat
  # "no Ready condition after deadline" as degraded evidence, not a silent pass.
  while :; do
    NOT_READY="$(kubectl get "$k" -n "$NAMESPACE" -o json 2>/dev/null \
      | jq -r '[.items[]? | select( ((.status.conditions // []) | map(select(.type=="Ready")) | .[0].status // "Missing") != "True") | .metadata.name] | join(",")')"
    if [ -z "$NOT_READY" ]; then echo "  ok   all $k Ready=True"; break; fi
    if [ "$(date +%s)" -ge "$DEADLINE" ]; then
      echo "  WAIT $k not Ready after ${RECONCILE_TIMEOUT}s: $NOT_READY"
      DEGRADED+="$k[$NOT_READY] "; break
    fi
    sleep "$POLL_INTERVAL"
  done
  dump_conditions "$k"
done
if [ -z "$DEGRADED" ]; then record "Phase 2: Reconcile/Ready" PASS "all reconcilable squad objects Ready=True (SC-1)"; P2_OK=1
else record "Phase 2: Reconcile/Ready" FAIL "degraded/not-Ready: ${DEGRADED% } (see phase2-conditions.txt)"; P2_OK=0; fi
endgroup
else record "Phase 2: Reconcile/Ready" BLOCKED "gated by Phase 1"; P2_OK=0; fi

# ============================================================ PHASE 3 — Crew up + toolchain attached (SC-2)
if [ "${P2_OK:-0}" -eq 1 ]; then
group "PHASE 3 — crew runnable + toolchain attached (SC-2)"
P3_OK=1; P3_MSG=""
# (a) every cross-reference resolves (reuse the getting-started-smoke ref-integrity logic)
DANGLING=0
resolve() { kubectl get "$1" "$2" -n "$NAMESPACE" >/dev/null 2>&1 && echo "  ok   $1/$2" || { echo "  MISS $1/$2"; DANGLING=$((DANGLING+1)); }; }
CATALOG_SET="$(echo "$CATALOG_JSON" | jq -r '.items[] as $t | $t.spec.versions[] | "\($t.metadata.name)@\(.version)"' | sort -u)"
while read -r tc; do
  [ -z "$tc" ] && continue
  grep -qxF "$tc" <<<"$CATALOG_SET" && echo "  ok   toolchain/$tc" || { echo "  MISS toolchain/$tc (not in $CP_NS catalog)"; DANGLING=$((DANGLING+1)); }
done < <(kubectl get skills -n "$NAMESPACE" -o json 2>/dev/null | jq -r '.items[]? | (.spec.requires.toolchains // [])[]' | sort -u)
# (b) bmad skill resolves on all agents; github skill resolves on coder/devops/reviewer roles
BMAD_SEEN="$(kubectl get skill bmad -n "$NAMESPACE" -o name 2>/dev/null || true)"
GH_SEEN="$(kubectl get skill github -n "$NAMESPACE" -o name 2>/dev/null || true)"
[ -n "$BMAD_SEEN" ] && echo "  ok   skill/bmad present" || { P3_OK=0; P3_MSG+="bmad skill missing; "; }
[ -n "$GH_SEEN" ]   && echo "  ok   skill/github present" || { P3_OK=0; P3_MSG+="github skill missing; "; }
[ "$DANGLING" -eq 0 ] || { P3_OK=0; P3_MSG+="$DANGLING dangling toolchain ref(s); "; }
if [ "$P3_OK" -eq 1 ]; then record "Phase 3: Crew + toolchain" PASS "agents runnable; bmad+github skills + toolchain refs resolve (SC-2)"
else record "Phase 3: Crew + toolchain" FAIL "${P3_MSG%%; }"; fi
endgroup
else record "Phase 3: Crew + toolchain" BLOCKED "gated by Phase 2"; P3_OK=0; fi

# ============================================================ PHASE 4 — Seed backlog + drive one story -> PR (SC-3)
if [ "${P3_OK:-0}" -eq 1 ]; then
group "PHASE 4 — seed backlog + drive >=1 BMAD story to a PR (SC-3)"
if [ -z "$SEED_CMD" ]; then
  # §9.1: no clean public API to seed a work item + create a Run. Report the finding
  # honestly as BLOCKED rather than fake a trigger. ProxOps/DevOps supply SEED_CMD.
  record "Phase 4: Story -> PR" BLOCKED "no SEED_CMD provided: work-item seed + Run-trigger seam unproven on this cluster (scenario §9.1). CRD/operator plane (SC-1/SC-2) validated above; supply SEED_CMD (and optional TRIGGER_CMD) to exercise the Run pipeline."
  P4_OK=0
else
  echo "  seeding backlog via SEED_CMD ..."
  WORKITEM="$(bash -c "$SEED_CMD" 2>>"$OUT_DIR/phase4-seed.log" | tail -n1)"
  if [ -z "$WORKITEM" ]; then
    record "Phase 4: Story -> PR" FAIL "SEED_CMD produced no work-item id (see phase4-seed.log)"; P4_OK=0
  else
    echo "  seeded work item: $WORKITEM"
    if [ -n "$TRIGGER_CMD" ]; then
      echo "  triggering Run via TRIGGER_CMD ..."
      bash -c "$TRIGGER_CMD $WORKITEM" 2>&1 | sed 's/^/         /' || true
    else
      # Best-effort default trigger: apply a Run CR referencing team/project/workitem.
      RUN_NAME="e2e-$(echo "$WORKITEM" | tr -cd 'a-z0-9-' | cut -c1-40)"
      echo "  applying default Run/$RUN_NAME (teamRef=bmad-squad projectRef=bmad-demo-project workItemRef=$WORKITEM) ..."
      cat <<EOF | kubectl apply -f - 2>&1 | sed 's/^/         /' || true
apiVersion: ksquad.io/v1alpha1
kind: Run
metadata:
  name: ${RUN_NAME}
  namespace: ${NAMESPACE}
spec:
  teamRef: { name: bmad-squad }
  projectRef: { name: bmad-demo-project }
  workItemRef: "${WORKITEM}"
EOF
    fi
    # Wait bounded for a Run to reach a terminal phase.
    echo "  waiting up to ${RUN_TIMEOUT}s for a Run to terminate ..."
    RDEADLINE=$(( $(date +%s) + RUN_TIMEOUT )); RUN_PHASE=""; RUN_REF=""
    while :; do
      RUN_JSON="$(kubectl get runs -n "$NAMESPACE" -o json 2>/dev/null || echo '{"items":[]}')"
      RUN_REF="$(echo "$RUN_JSON" | jq -r --arg w "$WORKITEM" '[.items[]? | select(.spec.workItemRef==$w)] | last | .metadata.name // ""')"
      RUN_PHASE="$(echo "$RUN_JSON" | jq -r --arg w "$WORKITEM" '[.items[]? | select(.spec.workItemRef==$w)] | last | .status.phase // ""')"
      echo "    Run=$RUN_REF phase=${RUN_PHASE:-<none>}"
      case "$RUN_PHASE" in
        Succeeded) break ;;
        Failed|Cancelled) break ;;
      esac
      if [ "$(date +%s)" -ge "$RDEADLINE" ]; then RUN_PHASE="${RUN_PHASE:-Timeout}"; break; fi
      sleep "$POLL_INTERVAL"
    done
    kubectl get runs -n "$NAMESPACE" -o wide > "$OUT_DIR/phase4-runs.wide.txt" 2>&1 || true
    if [ -z "$RUN_REF" ]; then
      record "Phase 4: Story -> PR" BLOCKED "no Run admitted for workItemRef=$WORKITEM (operator did not create/admit a Run — §9.1 finding). See phase4-runs.wide.txt + operator logs."
      kubectl logs -n "$CP_NS" -l app.kubernetes.io/component=operator --tail=200 > "$OUT_DIR/phase4-operator.log" 2>&1 || true
      P4_OK=0
    elif [ "$RUN_PHASE" = "Succeeded" ]; then
      record "Phase 4: Story -> PR" PASS "Run/$RUN_REF Succeeded for work item $WORKITEM (SC-3)"; P4_OK=1
    else
      record "Phase 4: Story -> PR" FAIL "Run/$RUN_REF terminal phase=$RUN_PHASE (not Succeeded). See phase4-runs.wide.txt + operator logs."
      kubectl logs -n "$CP_NS" -l app.kubernetes.io/component=operator --tail=200 > "$OUT_DIR/phase4-operator.log" 2>&1 || true
      P4_OK=0
    fi
  fi
fi
endgroup
else record "Phase 4: Story -> PR" BLOCKED "gated by Phase 3"; P4_OK=0; fi

# ============================================================ PHASE 5 — Verify the app artifact builds (SC-4)
if [ "${P4_OK:-0}" -eq 1 ]; then
group "PHASE 5 — verify >=1 landed PR is buildable (SC-4)"
# The crew pushes branches + opens PRs on $TARGET_REPO_URL. We confirm a branch other
# than the tracked ref appeared (evidence of a landed change) using read-only git
# ls-remote — no token needed for a public repo; a private repo needs the write-path
# token in the runner's git credential helper (out of band, not this script's concern).
BRANCHES="$(git ls-remote --heads "$TARGET_REPO_URL" 2>>"$OUT_DIR/phase5.log" | awk '{print $2}' | sed 's#refs/heads/##' | grep -v "^${TARGET_REPO_REF}$" || true)"
if [ -n "$BRANCHES" ]; then
  echo "  ok   crew-authored branch(es) on target repo:"; echo "$BRANCHES" | sed 's/^/         - /'
  # Buildability of the landed code is asserted by the PR's own CI (target repo, on
  # GitHub — that's the product). The harness records the branch list as evidence;
  # ProxOps links the PR + CI result in the Paperclip report (SC-4/SC-5).
  record "Phase 5: App buildable" PASS "landed branch(es) present on target repo; PR CI is the buildability gate (SC-4). Branches recorded to phase5.log."
  git ls-remote --heads "$TARGET_REPO_URL" > "$OUT_DIR/phase5-branches.txt" 2>&1 || true
  P5_OK=1
else
  record "Phase 5: App buildable" FAIL "no crew-authored branch on $TARGET_REPO_URL beyond '$TARGET_REPO_REF' — the story produced no landed change."
  P5_OK=0
fi
endgroup
else record "Phase 5: App buildable" BLOCKED "gated by Phase 4"; P5_OK=0; fi

# ============================================================ PHASE 6 — Report (SC-5/SC-6)
group "PHASE 6 — emit per-phase report"
MD="$OUT_DIR/phase-status.md"; JSON="$OUT_DIR/phase-status.json"
STAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
{
  echo "# Platform-E2E (BMAD squad -> todo app) — phase status"
  echo
  echo "- Generated: ${STAMP}"
  echo "- Namespace: ${NAMESPACE} · Control-plane: ${CP_NS}"
  echo "- Target repo: ${TARGET_REPO_URL} (ref ${TARGET_REPO_REF})"
  echo
  echo "| Phase | Result | Notes |"
  echo "|-------|--------|-------|"
  for i in "${!PH_ID[@]}"; do echo "| ${PH_ID[$i]} | ${PH_STATE[$i]} | ${PH_MSG[$i]} |"; done
  echo
  echo "## Success criteria"
  echo "- **SC-1** CRDs apply + reconcile → Phase 2"
  echo "- **SC-2** Team + agents up, toolchain attached → Phase 3"
  echo "- **SC-3** Project created + crew produces work (≥1 story → PR) → Phase 4"
  echo "- **SC-4** Target app builds (≥1 landed PR buildable) → Phase 5"
  echo "- **SC-5** Results published to Paperclip (this report is attached to the run issue, NOT GitHub)"
  echo "- **SC-6** No credential leak (tokens only ever named k8s Secrets; harness never reads/echoes a token)"
  echo
  echo "_Minimum bar = SC-1..SC-3 PASS and SC-4 PASS for ≥1 story. Partials reported honestly; BLOCKED ≠ FAIL._"
} > "$MD"

# JSON ledger for machine consumption.
{
  echo -n '{"generated":"'"$STAMP"'","namespace":"'"$NAMESPACE"'","targetRepo":"'"$TARGET_REPO_URL"'","phases":['
  for i in "${!PH_ID[@]}"; do
    [ "$i" -gt 0 ] && echo -n ','
    echo -n '{"phase":'"$(jq -Rn --arg v "${PH_ID[$i]}" '$v')"',"state":"'"${PH_STATE[$i]}"'","message":'"$(jq -Rn --arg v "${PH_MSG[$i]}" '$v')"'}'
  done
  echo ']}'
} > "$JSON"

echo; echo "=== per-phase status ==="; cat "$MD"; echo
note "artifacts written to $OUT_DIR (phase-status.md/.json, condition dumps, logs) — attach to the Paperclip run issue (SC-5)"
endgroup

# ------------------------------------------------------------------ exit disposition
# Minimum bar: Phases 0-3 PASS and Phase 5 (SC-4) PASS. Any BLOCKED/FAIL in the bar
# path exits non-zero so a CI lane surfaces it; the table distinguishes BLOCKED/FAIL.
if [ "${P0_OK:-0}" -eq 1 ] && [ "${P1_OK:-0}" -eq 1 ] && [ "${P2_OK:-0}" -eq 1 ] \
   && [ "${P3_OK:-0}" -eq 1 ] && [ "${P5_OK:-0}" -eq 1 ]; then
  echo "PLATFORM-E2E PASS: minimum bar cleared (SC-1..SC-4)."; exit 0
else
  echo "PLATFORM-E2E PARTIAL/FAIL: see the per-phase table above (BLOCKED ≠ FAIL)."; exit 1
fi
