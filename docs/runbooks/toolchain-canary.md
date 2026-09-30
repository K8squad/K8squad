# Runbook: all-tools sandbox canary (ISI-5222)

The **toolchain canary** is a repeatable smoke run that proves every registered
skill-toolchain binary is present and functional inside a **live warm-pool
sandbox** — the end-to-end validation of the `skill -> toolchain` init-pack path
fixed by ISI-5221 (`main@80af771b`, the layer-5 assembly/attach fix). Parent:
ISI-5219 (agent had no `git`/`curl` in the sandbox).

Run it after any operator roll, toolchain image rebuild, or new toolchain image
as a **fleet toolchain regression guard**.

## What it covers

The authoritative tool set is the 14 `Dockerfile.toolchain-*` images:

| | | | | |
|---|---|---|---|---|
| git | curl | gh | dtctl | kubectl |
| helm | jq | yq | python | node |
| go | uv | make | docker-cli | |

The list is kept in lockstep three ways, enforced by
`hack/toolchain-canary-drift.sh` (wire into CI): the canary body
(`hack/toolchain-canary.sh` `TOOLS=`), the Skill fixture
(`examples/toolchain-canary/skill.yaml`), and the `Dockerfile.toolchain-*` glob.
Adding a new toolchain image fails the drift guard until the canary is updated,
so no tool is silently missed.

> Note on `wget`: the parent issue text says "curl + wget", but there is no
> `Dockerfile.toolchain-wget` — `wget` is not a separately-registered toolchain.
> The canary asserts the 14 real toolchains; add a `wget` image + catalog entry
> first if it should be covered.

## Deploy precondition (verify before trusting a PASS)

The canary only exercises the fix if the **k8squad-system operator** is running
the ISI-5221 code (`main >= 80af771b`). Verify by **running-pod image digest**,
not tag — Harbor tags can be stale ([[k8squad-deploy-registry-ghcr-vs-harbor]],
[[isi-4745-resume-state]]):

```sh
export KUBECONFIG=~/.config/capmox/k8squad-test.kubeconfig   # read-only
kubectl get deploy -n k8squad-system ksquad-operator \
  -o jsonpath='{.spec.template.spec.containers[0].image}'; echo
kubectl get pods -n k8squad-system -l app.kubernetes.io/name=ksquad-operator \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.containerStatuses[0].imageID}{"\n"}{end}'
```

As of 2026-09-30 this is `10.0.0.13/k8squad/ksquad-operator:45e54e39` — the
**pre-fix** base. Until admin/ProxOps roll the operator to the merged sha, the
canary will (correctly) FAIL: sandbox pods are bare single-container pods with no
`/tools` init packs.

Also confirm the cluster Toolchain catalog is present (layers 1-4):

```sh
kubectl get toolchains.ksquad.io -n k8squad-system   # expect 14
```

## Running the canary (one-shot)

1. **Grant the canary Skill.** Apply `examples/toolchain-canary/skill.yaml` into
   the target squad namespace and attach it to a role (e.g. add
   `toolchain-canary` to a role's `spec.defaultSkills`). It declares all 14
   `requires.toolchains` refs plus the `dockerd` sidecar, so admission forces the
   operator to stage every init pack.

2. **Trigger a Run** whose task body is: run `hack/toolchain-canary.sh` and
   report its output. The script version-probes each tool, does a real
   `git clone` of `sympozium-todo-demo` + `git log -1`, and a `curl -sSf`. It
   exits non-zero (FAILs the Run) if any binary is missing or non-functional.

3. **Verify on the running sandbox pod** (belt-and-braces, before trusting the
   log): the init containers staged every pack, by digest:

   ```sh
   POD=<sandbox pod>; NS=<squad ns>
   kubectl get pod -n "$NS" "$POD" \
     -o jsonpath='{range .spec.initContainers[*]}{.name}{"\t"}{.image}{"\n"}{end}'
   # expect stage-git, stage-curl, stage-dtctl, ... one per toolchain
   ```

## Re-run wiring (regression guard)

- **Ad hoc**: re-trigger the Run above after any operator roll / image rebuild.
- **Scheduled**: register a Paperclip routine (or a fleet cron) that re-creates
  the canary Run on a cadence and alerts on a FAIL result. Keep the Skill fixture
  and `hack/toolchain-canary.sh` as the single source of truth so the drift guard
  keeps the routine honest.
- **CI**: run `hack/toolchain-canary-drift.sh` in the repo pipeline so the tool
  list can never drift from the `Dockerfile.toolchain-*` set.

## Pass/fail semantics

- **PASS**: `RESULT: PASS — every skill-toolchain present + functional`, exit 0.
- **FAIL**: `RESULT: FAIL`, exit 1, with a `MISSING/BROKEN:` list naming the
  offending tools. A FAIL on a correctly-rolled operator means an init pack did
  not stage (regression in the assembly/attach path) or an image is broken.
