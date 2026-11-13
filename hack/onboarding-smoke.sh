#!/usr/bin/env bash
# onboarding-smoke.sh — assert the human onboarding journey end-to-end on a
# FRESH install, so onboarding drift FAILS PRs (ISI-7270 / WS-7, plan rev
# 0cec0baa under ISI-7255).
#
# getting-started-smoke.sh proves install + example-squad apply with no
# dangling refs, but intentionally stops before the human journey. This
# sibling lane promotes the WS-2..5 milestone assertions (verified live on
# k8squad-test, ns k8squad-onboarding) into CI against whatever kube-context
# is selected (CI points it at a throwaway kind cluster):
#
#   1. Build the apiserver image from PR source (+ kind load) when docker/kind
#      are available, so /api/* regressions fail the PR — otherwise a
#      documented skip-with-reason and the published image is asserted.
#   2. Boot a disposable in-cluster Postgres (dev DSN path) and helm install
#      the LOCAL charts with controlPlane.enabled=true in the documented
#      apiserver-only topology (values.yaml: secondary/read-surface installs
#      set the non-apiserver components to replicas: 0) - the journey is
#      asserted at the API layer, so console/operator/memory/event-relay/
#      scm-webhook stay off: nothing pulls published component images; the
#      lane depends only on the kind-loaded PR-source apiserver + NATS.
#   3. Bootstrap the admin exactly as values.yaml documents it
#      (controlPlane.apiserver.bootstrapAdmin + existing Secret), then log in.
#   4. Run the adopter journey at the API layer (headlessly checkable):
#      Team create → member user → compose minimal-trio squad → dummy model
#      credential → Project + repo auth ref — asserting the
#      GET /api/onboarding/progress milestone flips ①→④ on a MEMBER session
#      (admin progress is N/A-complete per ISI-3932, asserted as its own
#      guard).
#   5. Milestone reads POLL-UNTIL-STABLE (default 240s): the onboarding read
#      model rides the informer cache, which serves a stale zero projection
#      for ~1 min after fresh-install writes (ISI-7352) — an instant assert
#      would flake.
#
# Credential/image-gated legs are documented skip-with-reason, never a silent
# pass (same convention as e2e.yml / getting-started-smoke.sh):
#   - LIVE LLM key + credential test-connection GREEN/RED against a real
#     provider (needs a real key + egress through the EgressPolicy allowlist)
#   - live GitHub repo-auth test (POST /api/projects/repo-auth/test — needs a
#     real PAT)
#   - operator reconcile-to-Ready of the running agents (BYO creds/images)
# The non-gated halves of those flows ARE asserted: the managed-credential
# Secret is created through the API and wired by name, and the Project CR
# carries spec.repo.auth.credentialSecretRef — exactly what milestone ④ reads
# (onboarding.go: milestone ③ checks credentialSecretRef presence, never the
# Secret payload; only a RECORDED failed test-connection un-completes it).
set -euo pipefail

# ------------------------------------------------------------------ config (overridable)
RELEASE="${RELEASE:-k8squad}"
CP_NS="${CP_NS:-k8squad-system}"                 # control-plane namespace (chart default)
CRDS_CHART="${CRDS_CHART:-config/helm-crds}"      # standalone CRD chart (ADR-0002 Option B)
CRDS_RELEASE="${CRDS_RELEASE:-k8squad-crds}"
CRDS_NS="${CRDS_NS:-k8squad-crds}"               # CRD chart's own release namespace
CHART="${CHART:-config/helm}"                    # control-plane / toolchain-catalog chart
PG_NAME="${PG_NAME:-k8squad-pg-onboarding}"      # disposable Postgres for the dev DSN path
PG_NS="${PG_NS:-k8squad-onboarding-db}"          # separate ns: the CP ns is chart-owned
PG_IMAGE="${PG_IMAGE:-postgres:16-alpine}"
DEFAULT_MODEL="${DEFAULT_MODEL:-claude-sonnet-4}"
STORAGE_CLASS="${STORAGE_CLASS:-standard}"        # kind default StorageClass
CRD_TIMEOUT="${CRD_TIMEOUT:-120s}"
INSTALL_TIMEOUT="${INSTALL_TIMEOUT:-10m}"
# The port-forward port defaults to a FREE ephemeral loopback port, never a fixed
# one: on the shared self-hosted runner a stale `kubectl port-forward` from a
# cancelled run can keep squatting a fixed port, and the journey would then be
# served by that foreign listener (observed in lane run 5: /api/* answered with the
# cookie but /admin/users 401ed). The script verifies OUR port-forward bound before
# trusting the port.
pick_free_port() {
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()' 2>/dev/null && return 0
  fi
  local p
  for _ in $(seq 1 50); do
    p=$(( (RANDOM % 20000) + 21000 ))
    if ! (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then echo "$p"; return 0; fi
  done
  echo 18080
}
API_PORT="${API_PORT:-$(pick_free_port)}"         # local port-forward port for ksquad-apiserver
API_URL="${API_URL:-http://127.0.0.1:${API_PORT}}"
KIND_CLUSTER="${KIND_CLUSTER:-chart-testing}"
API_IMAGE_TAG="${API_IMAGE_TAG:-onboarding-ci}"
API_IMAGE="${API_IMAGE:-ghcr.io/k8squad/ksquad-apiserver:${API_IMAGE_TAG}}"
# journey fixtures
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-onboarding-ci-admin-$(date +%s)}"
MEMBER_USER="${MEMBER_USER:-adopter-ci}"
MEMBER_PASSWORD="${MEMBER_PASSWORD:-onboarding-ci-member-x}"
TEAM_NAME="${TEAM_NAME:-starter-squad}"
PROJECT_NAME="${PROJECT_NAME:-onboarding-demo}"
PROJECT_REPO_URL="${PROJECT_REPO_URL:-https://github.com/K8squad/K8squad-website.git}"
PROGRESS_TIMEOUT="${PROGRESS_TIMEOUT:-240}"      # ISI-7352: stale zero projection ~1 min
POLL_INTERVAL="${POLL_INTERVAL:-5}"

fail() { echo "::error::$*" >&2; exit 1; }
note() { echo "::notice::$*" >&2; }
group() { echo "::group::$*"; }
endgroup() { echo "::endgroup::"; }

# ------------------------------------------------------------------ preflight
command -v kubectl >/dev/null || fail "kubectl not on PATH"
command -v helm    >/dev/null || fail "helm not on PATH"
command -v jq      >/dev/null || fail "jq not on PATH"
command -v curl    >/dev/null || fail "curl not on PATH"
kubectl cluster-info >/dev/null 2>&1 || fail "no reachable cluster on the current kube-context"
[ -d "$CHART" ]     || fail "chart not found: $CHART (run from repo root)"
[ -d "$CRDS_CHART" ] || fail "CRD chart not found: $CRDS_CHART (run from repo root)"
[ -f Dockerfile.apiserver ] || fail "Dockerfile.apiserver not found (run from repo root)"

BODY_FILE="$(mktemp /tmp/onboarding-smoke-body.XXXXXX)"
RUNTIME_DIR="$(mktemp -d /tmp/onboarding-smoke.XXXXXX)"
ACTIVE_JAR=""
SESSION_TOKEN=""   # explicit ksquad_session value extracted from the active jar
trap 'kill "${PF_PID:-0}" 2>/dev/null || true; rm -f "$BODY_FILE"; rm -rf "$RUNTIME_DIR"' EXIT
HTTP_CODE=""

# diag_auth LABEL - on an unexpected 401/403, dump everything needed to see what
# the apiserver actually received: the jar, a verbose /auth/me probe with the
# explicit cookie, the apiserver pods, the port-forward log, and the pod log tail.
diag_auth() {
  echo "::group::auth diagnostics ($*)"
  echo "--- active jar ($ACTIVE_JAR):"; cat "$ACTIVE_JAR" 2>/dev/null || echo "(none)"
  echo "--- GET /auth/me with the explicit cookie (verbose):"
  local diagbody; diagbody="$(mktemp /tmp/onboarding-smoke-diag.XXXXXX)"
  curl -sS --noproxy '*' -v -o "$diagbody" --max-time 10 \
    -H "Cookie: ksquad_session=$SESSION_TOKEN" "$API_URL/auth/me" 2>&1 | tail -25 || true
  echo "--- auth/me body: $(cat "$diagbody" 2>/dev/null)"; rm -f "$diagbody"
  echo "--- apiserver pods:"; kubectl -n "$CP_NS" get pods -o wide 2>/dev/null || true
  echo "--- port-forward log:"; tail -5 "${PF_LOG:-/dev/null}" 2>/dev/null || true
  echo "--- apiserver logs (tail):"; kubectl -n "$CP_NS" logs deploy/ksquad-apiserver --tail=40 2>/dev/null | tail -40 || true
  echo "::endgroup::"
}

# api METHOD PATH [JSON] → HTTP_CODE set, body on stdout via $BODY_FILE.
# --noproxy '*' keeps the loopback call off any runner-wide HTTP proxy; the session
# rides an explicit Cookie header (deterministic - no jar/secure-flag parsing quirks).
api() {
  local method="$1" path="$2" data="${3:-}"
  local args=(-sS --noproxy '*' -X "$method" "$API_URL$path" -o "$BODY_FILE" -w '%{http_code}' --max-time 30)
  if [ -n "$SESSION_TOKEN" ]; then
    args+=(-H "Cookie: ksquad_session=$SESSION_TOKEN")
  elif [ -n "$ACTIVE_JAR" ]; then
    args+=(-b "$ACTIVE_JAR")
  fi
  [ -n "$data" ]  && args+=(-H 'Content-Type: application/json' --data "$data")
  HTTP_CODE="$(curl "${args[@]}")"
  # Unexpected auth failure (the /auth/login wrong-password guard 401s on purpose) -
  # dump diagnostics once per path so the job log shows what the server saw.
  if { [ "$HTTP_CODE" = 401 ] || [ "$HTTP_CODE" = 403 ]; } && [ "$path" != "/auth/login" ]; then
    case " ${DIAG_DONE:-} " in *" $path "*) ;; *)
      DIAG_DONE="${DIAG_DONE:-} $path"; diag_auth "$method $path -> $HTTP_CODE" ;;
    esac
  fi
}

# login USERNAME PASSWORD -> per-user ksquad_session cookie jar (ACTIVE_JAR).
# The apiserver authenticates ONLY the forwarded ksquad_session cookie (server.go:
# the router trusts nothing but that cookie) - a Bearer-only call 401s, so every
# journey call rides the jar. Re-login switches the active jar per user.
login() {
  local u="$1"
  ACTIVE_JAR="$RUNTIME_DIR/cookies-$u.txt"
  HTTP_CODE="$(curl -sS --noproxy '*' -X POST "$API_URL/auth/login" -H "Content-Type: application/json" \
    -d "$(jq -nc --arg u "$u" --arg p "$2" "{username:\$u, password:\$p}")" \
    -c "$ACTIVE_JAR" -o "$BODY_FILE" -w "%{http_code}" --max-time 30)"
  [ "$HTTP_CODE" = 200 ] || fail "login as $u failed (HTTP $HTTP_CODE): $(cat "$BODY_FILE")"
  SESSION_TOKEN="$(awk '$6=="ksquad_session"{print $7}' "$ACTIVE_JAR" 2>/dev/null || true)"
  [ -n "$SESSION_TOKEN" ] \
    || fail "login as $u succeeded but set no ksquad_session cookie in $ACTIVE_JAR"
}
# poll_progress DONE STEP NEXT LABEL — GET /api/onboarding/progress until
# the projection matches, honoring the ISI-7352 informer-cache settle window.
poll_progress() {
  local want_done="$1" want_step="$2" want_next="$3" label="$4"
  local deadline=$((SECONDS + PROGRESS_TIMEOUT)) got=""
  while [ $SECONDS -lt $deadline ]; do
    local pargs=(-sS --noproxy '*' -o "$BODY_FILE" -w '%{http_code}' --max-time 30)
    if [ -n "$SESSION_TOKEN" ]; then pargs+=(-H "Cookie: ksquad_session=$SESSION_TOKEN"); else pargs+=(-b "$ACTIVE_JAR"); fi
    HTTP_CODE="$(curl "${pargs[@]}" "$API_URL/api/onboarding/progress")"
    [ "$HTTP_CODE" = 200 ] || fail "$label: progress read failed (HTTP $HTTP_CODE): $(cat "$BODY_FILE")"
    got="$(cat "$BODY_FILE")"
    if [ "$(jq -r .done <<<"$got")" = "$want_done" ] \
       && [ "$(jq -r .step <<<"$got")" = "$want_step" ] \
       && [ "$(jq -r '.nextMilestone // ""' <<<"$got")" = "$want_next" ]; then
      echo "  ok   $label → $got"
      return 0
    fi
    sleep "$POLL_INTERVAL"
  done
  fail "$label never reached done=$want_done step=$want_step next='${want_next}' within ${PROGRESS_TIMEOUT}s (ISI-7352 informer-lag window exceeded?) — last observation: $got"
}

echo "onboarding smoke: release=$RELEASE cp-ns=$CP_NS chart=$CHART crds-chart=$CRDS_CHART api=$API_URL"

# ============================================================ STEP 1 — apiserver image from PR source
group "STEP 1 — build the apiserver image from PR source (so /api/* regressions fail this PR)"
API_TAG_SET=()
if command -v docker >/dev/null && command -v kind >/dev/null; then
  docker build -f Dockerfile.apiserver -t "$API_IMAGE" . \
    || fail "docker build of Dockerfile.apiserver failed — PR source does not build"
  kind load docker-image "$API_IMAGE" --name "$KIND_CLUSTER" \
    || fail "kind load docker-image failed for $API_IMAGE (cluster $KIND_CLUSTER)"
  API_TAG_SET=(--set "controlPlane.image.tagOverrides.apiserver=$API_IMAGE_TAG")
  echo "apiserver image built from PR source and loaded into kind: $API_IMAGE"
else
  note "PR-source apiserver image skipped-with-reason: docker/kind not both on PATH — the journey below then asserts the PUBLISHED apiserver image (chart default tag). Catching /api/* regressions before release needs the docker+kind runner."
fi
endgroup

# ============================================================ STEP 2 — disposable Postgres (dev DSN path)
group "STEP 2 — boot disposable Postgres ($PG_IMAGE in $PG_NS)"
kubectl create namespace "$PG_NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl delete pod "$PG_NAME" -n "$PG_NS" --ignore-not-found >/dev/null
kubectl run "$PG_NAME" --namespace "$PG_NS" --image="$PG_IMAGE" \
  --env POSTGRES_USER=k8squad --env POSTGRES_PASSWORD=k8squad --env POSTGRES_DB=k8squad \
  --restart=Never
kubectl delete service "$PG_NAME" -n "$PG_NS" --ignore-not-found >/dev/null
kubectl expose pod "$PG_NAME" -n "$PG_NS" --port 5432 --name "$PG_NAME"
kubectl wait --for=condition=Ready "pod/$PG_NAME" -n "$PG_NS" --timeout=180s \
  || fail "disposable Postgres never became Ready — lane infra failure, not a product signal"
DSN="postgres://k8squad:k8squad@${PG_NAME}.${PG_NS}.svc.cluster.local:5432/k8squad?sslmode=disable"
endgroup

# ============================================================ STEP 3 — fresh install (local charts, control plane ON)
group "STEP 3 — helm install $CRDS_RELEASE then $RELEASE (controlPlane.enabled=true)"
helm upgrade --install "$CRDS_RELEASE" "$CRDS_CHART" \
  --namespace "$CRDS_NS" --create-namespace \
  --wait --timeout "$CRD_TIMEOUT" \
  || fail "CRDs chart install failed"
helm upgrade --install "$RELEASE" "$CHART" \
  --namespace "$CP_NS" --create-namespace \
  --set controlPlane.enabled=true \
  --set "controlPlane.nats.persistence.storageClassName=$STORAGE_CLASS" \
  --set controlPlane.operator.replicas=0 \
  --set controlPlane.console.replicas=0 \
  --set controlPlane.memory.replicas=0 \
  --set controlPlane.scmWebhook.replicas=0 \
  --set controlPlane.eventRelay.replicas=0 \
  --set-string "controlPlane.database.dsn=$DSN" \
  --set tools.defaultCatalog.enabled=true \
  --set "modelConfig.default.model=$DEFAULT_MODEL" \
  "${API_TAG_SET[@]}" \
  --wait --timeout "$INSTALL_TIMEOUT" \
  || fail "control-plane chart install failed"
mapfile -t CRDS < <(kubectl get crd -o name | grep 'ksquad.io$' || true)
[ "${#CRDS[@]}" -gt 0 ] || fail "no ksquad.io CRDs registered after helm install — chart drift"
for crd in "${CRDS[@]}"; do
  kubectl wait --for=condition=Established --timeout="$CRD_TIMEOUT" "$crd" \
    || fail "CRD not Established: $crd"
done
kubectl wait --for=condition=Available --timeout="$INSTALL_TIMEOUT" -n "$CP_NS" deploy/ksquad-apiserver \
  || fail "ksquad-apiserver Deployment never Available"
echo "fresh install up: ${#CRDS[@]} ksquad.io CRDs, control plane in $CP_NS"
endgroup

# ============================================================ STEP 4 — admin bootstrap + first login
group "STEP 4 — bootstrap admin (values.yaml controlPlane.apiserver.bootstrapAdmin) + login"
# The documented seed path: an existing Secret + bootstrapAdmin values on a
# helm upgrade (WS-2 / ISI-7265 verified live; ISI-7349 tracks the docs gap).
kubectl -n "$CP_NS" create secret generic ksquad-bootstrap-admin \
  --from-literal=password="$ADMIN_PASSWORD" --dry-run=client -o yaml | kubectl apply -f -
helm upgrade --install "$RELEASE" "$CHART" \
  --namespace "$CP_NS" \
  --set controlPlane.enabled=true \
  --set "controlPlane.nats.persistence.storageClassName=$STORAGE_CLASS" \
  --set controlPlane.operator.replicas=0 \
  --set controlPlane.console.replicas=0 \
  --set controlPlane.memory.replicas=0 \
  --set controlPlane.scmWebhook.replicas=0 \
  --set controlPlane.eventRelay.replicas=0 \
  --set "controlPlane.database.dsn=$DSN" \
  --set tools.defaultCatalog.enabled=true \
  --set "modelConfig.default.model=$DEFAULT_MODEL" \
  "${API_TAG_SET[@]}" \
  --set controlPlane.apiserver.bootstrapAdmin.enabled=true \
  --set "controlPlane.apiserver.bootstrapAdmin.username=$ADMIN_USER" \
  --set "controlPlane.apiserver.bootstrapAdmin.existingSecret=ksquad-bootstrap-admin" \
  --wait --timeout "$INSTALL_TIMEOUT" \
  || fail "bootstrap-admin helm upgrade failed"
kubectl rollout restart deploy/ksquad-apiserver -n "$CP_NS" >/dev/null 2>&1 || true
kubectl rollout status deploy/ksquad-apiserver -n "$CP_NS" --timeout="$INSTALL_TIMEOUT" >/dev/null \
  || fail "apiserver did not roll after bootstrap upgrade"

PF_LOG="$RUNTIME_DIR/port-forward.log"
kubectl port-forward -n "$CP_NS" svc/ksquad-apiserver "${API_PORT}:8080" >"$PF_LOG" 2>&1 &
PF_PID=$!
# Fail fast if OUR port-forward could not bind: a stale listener on the port would
# otherwise silently serve the journey from a foreign server.
for i in $(seq 1 30); do
  grep -q "Forwarding from" "$PF_LOG" && break
  if ! kill -0 "$PF_PID" 2>/dev/null; then
    fail "port-forward exited before binding ${API_PORT}: $(cat "$PF_LOG")"
  fi
  sleep 1
done
grep -q "Forwarding from" "$PF_LOG" \
  || fail "port-forward did not report 'Forwarding from' on ${API_PORT} - stale listener on the port? log: $(cat "$PF_LOG")"
for i in $(seq 1 60); do
  code="$(curl -sS --noproxy '*' -o /dev/null -w '%{http_code}' --max-time 2 "$API_URL/healthz" 2>/dev/null || true)"
  [ "$code" = "200" ] && break
  sleep 2
done
[ "${code:-}" = "200" ] \
  || fail "apiserver not reachable on $API_URL/healthz (code=${code:-none}); port-forward log: $(cat "$PF_LOG")"

login "$ADMIN_USER" "$ADMIN_PASSWORD"
echo "bootstrap admin logged in: $(jq -c . "$BODY_FILE")"
# Negative guard: the login wall must reject a wrong password (WS-2 verified).
api POST /auth/login "$(jq -nc --arg u "$ADMIN_USER" --arg p "definitely-wrong" '{username:$u, password:$p}')"
[ "$HTTP_CODE" = 401 ] || fail "wrong-password login must 401, got HTTP $HTTP_CODE: $(cat "$BODY_FILE")"
echo "  ok   wrong-password login rejected with 401"
endgroup

# ============================================================ STEP 5 — journey ①: Team + member zero-state
group "STEP 5 — milestone ① Team: create starter-squad + member, assert progress flips to done=1"
login "$ADMIN_USER" "$ADMIN_PASSWORD"
api POST /api/teams "$(jq -nc --arg n "$TEAM_NAME" '{name:$n}')"
[ "$HTTP_CODE" = 201 ] || [ "$HTTP_CODE" = 200 ] \
  || fail "POST /api/teams failed (HTTP $HTTP_CODE): $(cat "$BODY_FILE")"
TEAM_UID="$(kubectl get team "$TEAM_NAME" -n "$CP_NS" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
[ -n "$TEAM_UID" ] || fail "Team CR $TEAM_NAME not found in $CP_NS after POST /api/teams"
echo "  ok   Team $TEAM_NAME created (uid $TEAM_UID, home ns $CP_NS)"

# ISI-3924: creating the FIRST Team rebinds the bootstrap admin's dangling tenancy
# root to the new Team and REVOKES all of the admin's live sessions
# (composecrd.go rebindAdminTenancy -> RebindTeamID -> RevokeAllForUser; the apiserver
# logs "sessions invalidated, re-login required"). Re-login now so the member-create
# call below rides a live session minted against the rebound tenancy.
login "$ADMIN_USER" "$ADMIN_PASSWORD"
api POST /admin/users "$(jq -nc --arg u "$MEMBER_USER" --arg p "$MEMBER_PASSWORD" --arg t "$TEAM_UID" \
  '{username:$u, password:$p, globalRole:"user", teamId:$t}')"
[ "$HTTP_CODE" = 201 ] || fail "POST /admin/users failed (HTTP $HTTP_CODE): $(cat "$BODY_FILE")"
echo "  ok   member $MEMBER_USER created in team $TEAM_UID"

login "$MEMBER_USER" "$MEMBER_PASSWORD"
poll_progress 1 2 agents "milestone ① team (member view)"

# ISI-3932 guard: the bootstrap ADMIN's onboarding is N/A — reported 4/4 complete.
login "$ADMIN_USER" "$ADMIN_PASSWORD"
poll_progress 4 4 "" "milestone ②—④ admin N/A-complete guard (ISI-3932)"
endgroup

# ============================================================ STEP 6 — journey ②③: minimal-trio squad
group "STEP 6 — milestones ②+③ agents/models: compose minimal-trio (preset roles + shared credential ref)"
# The compose default credentialSecretRef is model-credentials/token (AD-5,
# matching examples/bmad-team). Admission resolves the ref fail-closed
# (agent_types.go), so the adopter's BYO-key stand-in Secret must exist first —
# CI uses a dummy token; the LIVE-key legs are the documented skip below.
kubectl -n "$CP_NS" create secret generic model-credentials \
  --from-literal=token=ci-dummy-model-token --dry-run=client -o yaml | kubectl apply -f -

# ISI-3924: the admin's tenancy root rebinds to the newest team at next login.
login "$ADMIN_USER" "$ADMIN_PASSWORD"
api POST /api/compose/squad "$(jq -nc '{template:"minimal-trio"}')"
if [ "$HTTP_CODE" = 207 ]; then
  fail "POST /api/compose/squad returned 207 with per-object errors: $(cat "$BODY_FILE")"
fi
[ "$HTTP_CODE" = 201 ] || [ "$HTTP_CODE" = 200 ] \
  || fail "POST /api/compose/squad failed (HTTP $HTTP_CODE): $(cat "$BODY_FILE")"
AGENTS_JSON="$(kubectl get agents -n "$CP_NS" -o json)"
for a in boss implementer manager; do
  row="$(jq -r --arg n "$a" '.items[] | select(.metadata.name==$n) \
    | [(.spec.roleRef.name // "MISSING"), (.spec.credentialSecretRef.name // "MISSING")] | @tsv' <<<"$AGENTS_JSON")"
  [ -n "$row" ] || fail "Agent $a missing after compose/squad"
  role="$(cut -f1 <<<"$row")"; cred="$(cut -f2 <<<"$row")"
  [ "$role" = "role-$a" ] || fail "Agent $a roleRef drift: expected role-$a, got $role"
  [ "$cred" = "model-credentials" ] || fail "Agent $a credentialSecretRef drift: expected model-credentials, got $cred"
  echo "  ok   Agent $a (roleRef=role-$a, credentialSecretRef=model-credentials)"
done
login "$MEMBER_USER" "$MEMBER_PASSWORD"
poll_progress 3 4 project "milestones ②+③ agents+models (member view)"
note "credential-gated legs skipped-with-reason: LIVE LLM key test-connection (GREEN on a real provider / RED un-completing milestone ③) and primary+fallback round-trip — verified manually in WS-4 (ISI-7267); needs a real key + egress through the EgressPolicy allowlist. The non-gated half (shared credentialSecretRef wiring + milestone ③ flip) IS asserted above."
endgroup

# ============================================================ STEP 7 — journey ④: Project + repo auth ref
group "STEP 7 — milestone ④ project: create onboarding-demo + wire repo auth credentialSecretRef"
login "$ADMIN_USER" "$ADMIN_PASSWORD"
api POST /api/projects "$(jq -nc --arg n "$PROJECT_NAME" --arg u "$PROJECT_REPO_URL" \
  '{name:$n, repo:{url:$u}}')"
[ "$HTTP_CODE" = 201 ] || [ "$HTTP_CODE" = 200 ] \
  || fail "POST /api/projects failed (HTTP $HTTP_CODE): $(cat "$BODY_FILE")"
echo "  ok   Project $PROJECT_NAME created"

# Managed credential through the API (paste-key stand-in — dummy value; the
# LIVE GitHub test is the documented skip below). Falls back to an equivalent
# labeled Secret if the endpoint declines a non-live-verifiable value.
CRED_SECRET=""
api POST /api/credentials "$(jq -nc --arg v "ci-dummy-github-token" --arg p "$PROJECT_NAME" \
  '{type:"github", projectId:$p, value:$v}')"
if [ "$HTTP_CODE" = 201 ] || [ "$HTTP_CODE" = 200 ]; then
  CRED_SECRET="$(jq -r '.name // .secretName // .ref // empty' "$BODY_FILE")"
  echo "  ok   managed credential created via POST /api/credentials: $CRED_SECRET"
fi
if [ -z "$CRED_SECRET" ]; then
  note "POST /api/credentials did not return a secret name (HTTP $HTTP_CODE) — falling back to an equivalently-labeled Secret: $(cat "$BODY_FILE")"
  CRED_SECRET="proj-${PROJECT_NAME}-cred-github"
  kubectl -n "$CP_NS" create secret generic "$CRED_SECRET" \
    --from-literal=token=ci-dummy-github-token --dry-run=client -o yaml \
    | kubectl label --local -f - ksquad.io/managed-credential=true ksquad.io/credential-type=github -o yaml --overwrite \
    | kubectl apply -f -
fi

api PUT "/api/projects/$PROJECT_NAME" "$(jq -nc --arg n "$PROJECT_NAME" --arg u "$PROJECT_REPO_URL" --arg c "$CRED_SECRET" \
  '{name:$n, repo:{url:$u, auth:{credentialSecretRef:{name:$c}}}}')"
[ "$HTTP_CODE" = 200 ] || [ "$HTTP_CODE" = 201 ] \
  || fail "PUT /api/projects/$PROJECT_NAME (repo auth ref) failed (HTTP $HTTP_CODE): $(cat "$BODY_FILE")"
WIRED="$(kubectl get project "$PROJECT_NAME" -n "$CP_NS" -o jsonpath='{.spec.repo.auth.credentialSecretRef.name}')"
[ "$WIRED" = "$CRED_SECRET" ] \
  || fail "Project CR spec.repo.auth.credentialSecretRef drift: expected $CRED_SECRET, got '$WIRED'"
echo "  ok   Project $PROJECT_NAME carries spec.repo.auth.credentialSecretRef=$CRED_SECRET (milestone ④ input)"

login "$MEMBER_USER" "$MEMBER_PASSWORD"
poll_progress 4 4 "" "milestone ④ project — journey complete 4/4 (member view)"
note "credential-gated leg skipped-with-reason: LIVE repo-auth test (POST /api/projects/repo-auth/test + Team CR ksquad.io/onboarding-test-connection-repo=passed) needs a real GitHub PAT — verified manually in WS-5 (ISI-7268). The non-gated half (credentialSecretRef wiring, the exact input milestone ④ reads) IS asserted above."
note "image-gated leg skipped-with-reason: operator reconcile-to-Ready of the running agents — same skip-with-reason as getting-started-smoke.sh STEP 5."
endgroup

echo "onboarding smoke PASSED: fresh install + admin bootstrap + journey ①→④ complete (done=4/4 on a member session), with documented skip-with-reason on the credential/live-endpoint legs."
