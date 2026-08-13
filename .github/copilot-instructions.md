# Copilot review instructions — K8squad control plane

These instructions guide GitHub Copilot code review for every pull request in this
repository. Copilot should apply them in addition to its general review behaviour.
Prioritise **correctness, security, and the architectural invariants below** over
style nits — style is already enforced by `golangci-lint` and ESLint in CI.

The module is `github.com/K8squad/K8squad`: a Kubebuilder-based Kubernetes control
plane (operator + apiserver + memory + console + runtime shims) plus the
coordination spine that dispatches and reclaims agent Runs.

When you flag an issue, cite the file and line, explain the impact, and prefer a
concrete suggested change. When a diff looks correct against these rules, say so
briefly rather than inventing nits.

## 1. Go conventions

- **golangci-lint must stay green.** The repo enables `govet`, `staticcheck`,
  `errcheck`, `ineffassign`, `unused`, `revive` (`exported` rule on), `gosec`,
  `misspell`, `unconvert`, and `bodyclose` (see `.golangci.yml`). Flag code that
  would trip these — especially unchecked errors (`errcheck`), unhandled
  `resp.Body`/rows `Close()` (`bodyclose`), and missing doc comments on exported
  identifiers (`revive: exported`).
- **Error handling:** never discard errors with `_` on a fallible call unless the
  reason is obvious and commented. Wrap with `fmt.Errorf("...: %w", err)` to
  preserve the chain; do not `panic` in library/reconciler/request paths; do not
  log-and-return the same error twice.
- **Context propagation:** any function doing I/O, a DB query, a Kubernetes API
  call, or an outbound request must accept and honour a `context.Context` as its
  first parameter and pass it through — no `context.TODO()`/`context.Background()`
  in request or reconcile paths (only at genuine entrypoints). Respect
  cancellation and deadlines; do not swallow `ctx.Err()`.
- Prefer table-driven tests, `t.Context()`, and `require`/`assert` from testify to
  match the existing style.

## 2. CRD / API type changes (`api/**`, `config/crd/**`)

- Any change to a `*_types.go` under `api/` that alters a Go struct **must** be
  accompanied by regenerated `zz_generated.deepcopy.go` (`make generate`) and
  regenerated CRD manifests under `config/crd/bases` (`make manifests`). Flag a PR
  that edits API structs but does not update the generated DeepCopy methods or CRD
  YAML — the two will drift and the generated artifacts will no longer match the
  source types.
- New or changed spec fields should carry **CEL validation** (`+kubebuilder:validation:XValidation`)
  and/or standard kubebuilder validation markers (`Required`, `Enum`, `Minimum`,
  `MaxLength`, immutability rules) rather than validating only in the controller.
  Flag user-settable fields that have no validation markers, and immutable fields
  (e.g. identity/reference keys) that lack an immutability CEL rule.
- Do not hand-edit `zz_generated.deepcopy.go` or generated CRD bases; changes there
  should come from code generation only.

## 3. Coordination spine — the no-P2P invariant (§6)

The coordination spine is **hub-mediated by design**. Per the §6 no-P2P
architectural invariant, agents and Runs coordinate **only** through the control
plane (apiserver / coordination store / dispatch + reclaim path). Flag as a
**blocking** concern any change that introduces a peer-to-peer path, e.g.:

- one agent/shim/worker opening a direct network connection to another agent/pod to
  exchange work, claims, or memory instead of going through the apiserver;
- service discovery or addressing that lets Runs talk to each other directly;
- bypassing the claim / fence-token / dispatch-marker flow to hand work between
  workers out-of-band.

Coordination correctness also depends on the claim/reclaim safety rules: a single
active claim per item, monotonic **fence tokens** (stale holders must be rejected,
`fence_token + 1` on reclaim), `SKIP LOCKED` fan-out that never double-claims, and a
deterministic dispatch marker so a Run executes **exactly once**. Flag changes that
weaken any of these.

## 4. RBAC — deny-by-default middleware

Authorization is **deny-by-default**. Every new HTTP/API endpoint (route, handler,
gRPC method) **must** pass through the central authorization middleware; access must
be explicitly granted, never implicit. Flag any new endpoint or handler that:

- is registered without going through the shared auth/authorization middleware, or
- reads/mutates state before the authorization check runs, or
- introduces an "allow by default", unauthenticated, or `TODO: add auth` path.

Also flag broadened Kubernetes RBAC (`+kubebuilder:rbac` markers and any generated
RBAC manifests) that grants more verbs or resources than the change needs — keep it
least-privilege.

## 5. Tests are required for coordination, reclaim, and sandbox code

Any PR that adds or changes logic in the **coordination**, **reclaim/claim/fence**,
or **sandbox/runtime-isolation** areas must include tests that cover it. For these
areas specifically, flag missing tests as a blocking concern — including the
concurrency edge cases (parallel claimers, crash-mid-claim reclaim, stale-holder
write rejection, double-dispatch prevention). New reconcilers/handlers elsewhere
should also ship with tests; note when they don't.

## 6. No secrets or credentials in code

- Flag any hard-coded secret, token, password, API key, private key, kubeconfig,
  connection string with credentials, or bearer token in source, manifests,
  Dockerfiles, workflows, or test fixtures — including realistic-looking dummies.
- Configuration that needs a secret must reference a Kubernetes **Secret**
  (`secretRef` / `valueFrom.secretKeyRef` / mounted volume), never an inline literal
  value. Flag env vars or CRD fields that carry a raw credential value instead of a
  Secret reference.
- Do not log secret values or full auth headers.

## Out of scope / low priority

- Formatting, import ordering, and lint-autofixable style — CI already enforces it;
  don't spend review budget there.
- Generated files (`zz_generated.*`, CRD bases, mocks) — review the generator input,
  not the generated output.
- Do not reference internal ticket IDs or planning documents in review comments;
  keep feedback about the code in this repository.
