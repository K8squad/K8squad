# A2A Shim Conformance Suite (Story 5.6, `[GATE-BLOCKING]`)

A runnable suite a vendor can execute **independently** to prove an agent-runtime
shim "works in any squad, zero core changes." A shim that passes is safe for the
KSquad core reconciler to drive, because every A2A invariant the core relies on
has been asserted here.

> Arch §7.5, §11.2 · spec §3/§4/§5/§6 · FR-D5 · deps ISI-2114 (shim seam),
> ISI-2891 (v1 shim set). The $0 Ollama lane is the free CI path of ISI-2157.

## What it checks

Six dimensions, each a required part of the verdict (a shim is conformant only
when **all six** pass, on **both lanes** it is eligible for):

| Check | What it asserts |
|-------|-----------------|
| `agent-card-validity`   | Schema `ksquad.a2a/v1`, registered runtime type, pinned CLI + protocol (A2A/MCP) revisions, model with a positive context window, mandatory `streaming`, ≥1 advertised artifact kind (spec §6.1). |
| `task-lifecycle`        | `submitted → working → completed`; a re-`SubmitTask` on the same id reattaches without a second execution (**C1**); `CancelTask` drains a live task to `canceled` and is an idempotent no-op on a terminal/unknown task (**C8**). |
| `sse-progress`          | SSE events are gap-free, monotonic from seq 1, first is `submitted` and last is a terminal status; a resume from any seq replays only later events gap-free (**C4**). |
| `artifact-emission`     | Every `artifact-ref` is content-addressed (64-hex sha256) and bound to the Run's `work_item_id` (spec §5). |
| `capability-honesty`    | The runtime exercises no capability its Agent Card did not advertise — no unadvertised artifact kind, no tool events without `toolCalls`, no `input-required` without `interactivePrompt` (**F15/C6**). |
| `credential-metadata`   | The auth block is metadata only (a Secret **reference**, a known shape); the raw credential never appears on the card or the SSE wire (**NFR-SEC3**). |

## Lanes

- **`default`** — the runtime rides its own fixed-vendor wire.
- **`ollama`** — the runtime's model is resolved to a **BYO Ollama endpoint**
  (stories 5.7/5.8). The same six checks run, plus the credential check proves
  the model wire uses the zero-cost placeholder key (`OPENAI_API_KEY=ollama`)
  against `OPENAI_BASE_URL`, carrying **no paid provider credential** — a vendor's
  **$0** way to prove conformance. Only runtimes advertising `byoModelEndpoint`
  are eligible; an ineligible runtime is refused, not silently passed.

The suite drives the real `pkg/shim` engine through a **scripted runner**, so it
needs no live coding-agent CLI and the Ollama lane asserts the model-wire *shape*
with **no live Ollama server** — it runs green in a $0 CI lane.

## Run it

```bash
# Every registered runtime, native wire:
go run ./cmd/conformance

# One runtime, the $0 Ollama lane:
go run ./cmd/conformance -runtime opencode -lane ollama -ollama-model qwen3

# Machine-readable report (exit code 1 if any runtime fails any check):
go run ./cmd/conformance -json

# The full gate (unit + race + both lanes):
make conformance
```

CI runs it as the required `conformance` check (`.github/workflows/conformance.yml`).

## Certify your own runtime (zero core change)

A vendor adds a runtime by implementing `runtimes.Runtime` and calling
`runtimes.Register` — then certifies it with the same suite:

```go
import "github.com/K8squad/K8squad/conformance"

rep := conformance.VerifyRuntime(myRuntime, conformance.Options{Lane: conformance.LaneOllama})
if !rep.OK() {
    log.Fatalf("not conformant:\n%s", rep)
}
```

`VerifyRuntime` never returns an error: every failure is a `Result` on the
`Report`, so one call gives the full picture. Registering the runtime is the only
core-adjacent step — the engine, SSE sequencing and lifecycle are shared and
already conformant.
