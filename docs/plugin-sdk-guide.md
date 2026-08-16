# Plugin SDK guide — building KSquad extensions

**Audience:** developers integrating KSquad with their own internal systems —
notification hubs, data warehouses, memory backends, metrics pipelines, external
trackers.

A **plugin** is an out-of-process program that *observes* what a squad does. It
subscribes to a stream of domain events over a **NATS/JetStream** event bus and reacts
— posting to Slack, exporting metrics, mirroring runs into your own tooling, streaming
memory writes into a knowledge graph. Plugins are how you extend KSquad **without
forking the control plane**.

> **Status: alpha (`v1alpha1`).** The event seam, the subject taxonomy, and the plugin
> contract described here are locked by [ADR-023](#references) and stable in *shape*,
> but the concrete artifacts — the NATS subchart (Story 9.4), the outbox relay, and the
> first-party `pkg/events` catalog (Epic 12) — are landing incrementally. Event payload
> schemas are versioned and will be additive-or-gated (never ambient breakage), but pin
> a catalog rev and expect fields to be *added* before `v1`. Where a component has not
> landed yet, this guide says so.

## Table of contents

1. [Plugin overview — what plugins can and can't do](#1-plugin-overview)
2. [Architecture — the event bus, lifecycle, and sandboxing](#2-architecture)
3. [Your first plugin](#3-your-first-plugin)
4. [Event reference](#4-event-reference)
5. [Plugin SDK API](#5-plugin-sdk-api)
6. [Examples](#6-examples)
7. [Testing plugins](#7-testing-plugins)
8. [Publishing](#8-publishing)
9. [References](#references)

---

## 1. Plugin overview

### What plugins CAN do

- **Observe domain events.** Every meaningful state change in a squad — a run starts, a
  work item is claimed, an artifact is produced, memory is written, a commit lands —
  is published to the event bus. A plugin subscribes and reacts.
- **Integrate external systems.** Turn events into Slack messages, PagerDuty alerts,
  Datadog metrics, rows in your warehouse, tickets in your own tracker.
- **Fan work out.** Stream memory writes into an external knowledge graph (this is
  exactly how the GRAIL memory backend plugin works — see [§6](#6-examples)).
- **Catch up on missed events.** JetStream retains events, so a plugin that was down can
  replay from where it left off instead of losing history.
- **Act on the world through the public API.** If your plugin needs to *write*
  somewhere (mirror a run into Jira, say), it does so as an ordinary authenticated API
  client — the same public REST/SSE surface any principal uses — carrying its own
  credentials. That path is fully audited.

### What plugins CANNOT do

Plugins are **observers, not a coordination path.** This is a deliberate, load-bearing
guarantee — the same rule that keeps agent memory and the discussion room from becoming
back-channels, applied a third time to plugins.

- **They cannot coordinate.** There is no plugin affordance to **claim**, **lease**,
  **fence**, or **hand off** a work item. Custody transfer lives only in the fenced
  coordination record inside the control plane; the event seam exposes no such surface.
- **They cannot mutate state.** The seam is **emit-only, one-way** (`outbox → NATS →
  plugin`). *Nothing a plugin publishes on NATS re-enters* the coordination or memory
  transaction. Publishing to a `ksquad.*` subject does **not** move a work item, resume
  a run, or write memory — the relay never flows NATS → control plane.
- **They cannot block the platform.** A slow, crashing, or absent plugin — or a NATS
  outage — **can never stall a run, a claim, or a memory write.** The relay is decoupled
  from the write path; the worst a broken plugin causes is delayed fan-out to *itself*.
- **They cannot embed in the console.** v1 plugins are headless backend workers. There
  is no in-console UI surface for third-party plugins.
- **They are untrusted.** Plugin code runs least-privilege and outside the trust
  boundary. It gets events and its own scoped credentials — nothing more.

> **The mental model:** a plugin is a **read replica of squad activity with a side
> effect.** If you find yourself wanting a plugin to *decide who does work next*, that
> is a control-plane concern, not a plugin — the answer is a CRD or an API call, not an
> event subscriber.

---

## 2. Architecture

### 2.1 Data in Postgres, events on NATS

KSquad keeps **all durable state in a single Postgres** — the coordination record,
memory, discussion, work items, and artifacts. That is the source of truth and never
moves. What flows to plugins is a *copy* of each state change, delivered over a
**NATS/JetStream** bus.

```
  ┌─────────────────────── control plane (trusted) ───────────────────────┐
  │                                                                        │
  │   state change  ──┐                                                    │
  │   (run, claim,    │  same transaction                                  │
  │    memory, …)     ▼                                                     │
  │            ┌─────────────┐        ┌──────────────┐                     │
  │            │  Postgres   │        │    relay     │  tails outbox       │
  │            │  ┌───────┐  │        │   worker     │  (LISTEN/NOTIFY     │
  │            │  │ state │  │        │              │   + poll),          │
  │            │  ├───────┤  │───────▶│  publishes   │  stamps             │
  │            │  │outbox │  │        │  to NATS,    │  published_at,      │
  │            │  └───────┘  │        │  re-publishes│  at-least-once      │
  │            └─────────────┘        │  unflushed   │                     │
  │             source of truth       └──────┬───────┘                     │
  └────────────────────────────────────────┼─────────────────────────────┘
                                            ▼
                              ┌───────────────────────────┐
                              │      NATS / JetStream      │  replayable subjects
                              │  ksquad.{entity}.{project} │  (catch-up buffer,
                              │      .{squad}.{event}      │   NOT state of record)
                              └─────────────┬─────────────┘
                                            ▼  nats_sub(subject)   one-way ▲ never back
                              ┌───────────────────────────┐
                              │   YOUR PLUGIN (untrusted)  │  out-of-process
                              │   sidecar or standalone    │  read-only observer
                              │   BYO credentials          │  least-privilege
                              └───────────────────────────┘
```

The key seam is the **transactional outbox**:

1. When the control plane commits a state change, it writes the corresponding event as
   an append-only row to a Postgres `outbox` table **in the same transaction**. So an
   event exists **if and only if** its state change committed — no lost events, no
   phantom events.
2. A **relay worker** tails the outbox (`LISTEN/NOTIFY` plus polling), publishes each
   event to its NATS subject, and stamps the row `published_at`. If a publish fails, or
   NATS is down, it **re-publishes unflushed rows** on recovery — delivery is
   **at-least-once**, with the outbox as the durable retry buffer. No dual-write hole.
3. The relay runs **outside** the reconcile/coordination transaction. That is the
   isolation crux: **NATS being unavailable only delays fan-out — it never blocks a
   run, a claim, or a memory write.** You can even install KSquad with `nats.enabled=false`;
   the core comes up and buffers events in the outbox.

> **At-least-once, so design idempotent handlers.** A plugin may see the same event
> more than once (relay restart, replay, redelivery). Deduplicate on the event's stable
> identity — see [§5.4](#54-idempotency-and-error-handling).

### 2.2 Subject taxonomy

Subjects are hierarchical, so you subscribe with NATS wildcards to exactly the slice you
care about:

```
ksquad.{entity}.{project}.{squad}.{event_type}
```

| Token | Meaning | Example |
|-------|---------|---------|
| `entity` | The domain object the event is about | `run`, `workitem`, `artifact`, `memory`, `scm` |
| `project` | The Project the event belongs to | `acme` |
| `squad` | The Team/squad within the project | `backend` |
| `event_type` | The specific transition | `started`, `succeeded`, `claimed`, `registered` |

Wildcards (`*` = one token, `>` = the rest):

| Subscription | Matches |
|--------------|---------|
| `ksquad.run.acme.backend.started` | one squad's run-started events |
| `ksquad.run.acme.>` | every run event on project `acme` |
| `ksquad.run.*.*.succeeded` | all successful runs, every project/squad |
| `ksquad.*.acme.>` | *everything* on project `acme` |
| `ksquad.>` | the whole firehose (test/dev only — filter in production) |

Subjects are part of the **versioned event catalog** ([§10.2 drift discipline](#references)):
a subject's shape and its payload schema evolve additively-or-gated, so a pinned plugin
survives platform upgrades.

### 2.3 Plugin lifecycle

A plugin is **registered and configured per Project/squad** — you don't install one
global plugin, you attach a plugin to the squads whose activity it should observe.

```
  register ──▶ configure ──▶ connect ──▶ subscribe ──▶ observe/react ──▶ (replay on restart)
   (per         (CRD +        (NATS       (subject      (idempotent        (JetStream
    squad)       Secret)       creds)      + wildcard)    handler)           catch-up)
```

1. **Register/configure** — declare the plugin against a Project/squad via a
   configuration CRD (see [§5.3](#53-configuration-via-crd)), including a reference to
   the NATS connection and the plugin's own outbound credentials.
2. **Connect** — the plugin connects to the JetStream bus with its scoped credentials.
3. **Subscribe** — it opens a durable JetStream consumer on its subject(s). A *durable*
   consumer remembers its position, so a restart resumes rather than restarts.
4. **Observe & react** — for each event it decodes the payload and runs its side effect.
5. **Replay** — after downtime, JetStream redelivers from the last acknowledged
   position. Core (non-JetStream) subscriptions are fire-and-forget with no catch-up —
   use JetStream when you can't afford to miss events.

### 2.4 Sandboxing & trust

Plugins are **untrusted** and run least-privilege, out-of-process:

- **Out-of-process, per Project/squad.** A plugin is a sidecar or a standalone service —
  never in-process with the control plane. A crash or a hang is contained.
- **Read-in via events, write-out via public APIs.** The only inbound channel is the
  NATS event stream. Any outbound action goes through the same authenticated, audited
  public API surface as any other client — with **no coordination primitive** available.
- **Bring-your-own credentials (BYO Secret).** A plugin that calls an external system
  carries a **per-Project / per-user Secret reference** — never a shared master
  credential. Your Slack token, your warehouse DSN, your PagerDuty key live in *your*
  Secret, scoped to the squad the plugin serves.
- **Observability.** Outbox depth, unflushed-event lag, NATS publish failures, and
  JetStream consumer lag are first-class OTel metrics on the platform side, so operators
  can see when a plugin is falling behind.

---

## 3. Your first plugin

A hello-world plugin that reacts when a run starts. The walk-through is
**transport-first**: prove you can receive an event with the `nats` CLI, then decode it
with typed Go, then run it out-of-process.

### Prerequisites

- A running KSquad install with the NATS/JetStream bus (Story 9.4) reachable, or a local
  NATS for development (`nats-server -js`, see [§7](#7-testing-plugins)).
- The [`nats` CLI](https://github.com/nats-io/natscli) for the smoke test.
- Go 1.23+ for the typed version.

### Step 1 — Subscribe with any NATS client

Before writing a line of Go, confirm events are flowing. Subscribe to every run event on
project `acme`:

```bash
nats sub "ksquad.run.acme.>"
```

Kick off a run in that project and you'll see frames land — `ksquad.run.acme.backend.started`,
`…running`, `…succeeded`. This is the whole contract: **a subject and a JSON payload.**
Any language with a NATS client can be a plugin.

### Step 2 — Decode with the typed catalog

For real plugins, decode into typed structs from the first-party Go catalog rather than
hand-parsing JSON. Create `observer.go`:

```go
package main

import (
	"log"

	"github.com/nats-io/nats.go"
	"github.com/K8squad/K8squad/pkg/events" // first-party typed catalog (Epic 12)
)

func main() {
	nc, err := nats.Connect(nats.DefaultURL) // dev; use creds + JetStream in prod
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer nc.Drain()

	// Subscribe to every run event on project "acme".
	_, err = nc.Subscribe("ksquad.run.acme.>", func(m *nats.Msg) {
		// Decode only the events you care about; skip the rest.
		ev, err := events.Decode[events.RunStarted](m.Data)
		if err != nil {
			return // not a RunStarted (or a schema you don't handle) — ignore
		}
		log.Printf("run %s started on %s/%s", ev.RunID, ev.Project, ev.Squad)
		// ...your side effect here (notify, export, mirror)...
	})
	if err != nil {
		log.Fatalf("subscribe: %v", err)
	}

	select {} // block forever
}
```

> **`pkg/events` is landing with Epic 12.** Until it ships, decode payloads yourself
> with `encoding/json` against the [event reference](#4-event-reference) below — the
> wire format is stable JSON; the typed catalog is an ergonomic layer over it, not a
> different contract.

### Step 3 — Run it out-of-process

```bash
go run ./observer     # a sidecar next to the squad, or a standalone service
```

That's a complete plugin: subscribe → decode → react, running as its own process,
read-only, unable to block the squad it watches.

---

## 4. Event reference

All events share a common envelope and an event-type-specific payload. Subjects follow
`ksquad.{entity}.{project}.{squad}.{event_type}` ([§2.2](#22-subject-taxonomy)).

### 4.1 Envelope

Every event carries, alongside its typed payload:

| Field | Meaning |
|-------|---------|
| `eventId` | Stable unique id — **dedupe on this** (at-least-once delivery). |
| `type` | Event type + catalog schema version (e.g. `run.started@v1`). |
| `project`, `squad` | Tenancy coordinates (also in the subject). |
| `occurredAt` | When the state change committed (Postgres txn time). |
| `traceId` | OTel trace id, so plugin side effects correlate to the run. |

### 4.2 Event catalog

The taxonomy maps 1:1 onto existing platform state. Payload fields below are the stable
core; treat additional fields as *additive* over time.

| Subject `entity.event_type` | When it fires | Payload (core fields) |
|---|---|---|
| `run.started` | An agent begins executing a run | `{ runId, project, squad, agent }` |
| `run.running` | Run is actively executing | `{ runId, project, agent }` |
| `run.succeeded` | Terminal success | `{ runId, phase, initiatedBy }` |
| `run.failed` | Terminal failure | `{ runId, phase, reason }` |
| `run.cancelled` | Run cancelled | `{ runId, reason }` |
| `run.paused` | Run paused (e.g. credential expiry, rate limit) | `{ runId, reason, resumeAt? }` |
| `workitem.created` | A work item/ticket was created | `{ workItemId, project, squad }` |
| `workitem.claimed` | An agent took the item | `{ workItemId, runId, agent }` |
| `workitem.handoff` | Control-plane re-dispatch (fenced) | `{ workItemId, from, to }` |
| `workitem.completed` | A work item finished | `{ workItemId, runId }` |
| `artifact.registered` | An artifact (build/diff/blob) was produced | `{ workItemId, runId, kind }` |
| `memory.written` | An agent persisted provenanced memory | `{ agent, scope, key }` |
| `scm.pushed` | A commit / CI status event | `{ repo, sha, status }` |
| `scm.check_run.failed` | A CI check failed | `{ repo, sha, checkRun, project, squad }` |

Notes on the taxonomy that trip people up:

- **`run.paused`** covers both credential-expiry pauses and LLM rate-limit pauses. A
  `resumeAt` may be present when the platform scheduled a timed resume. **A plugin
  observes this; it never injects the credential or resumes the run** — that stays the
  fenced control-plane path.
- **`workitem.handoff`** is a *control-plane* re-dispatch you get to *watch*. It is not a
  primitive you can invoke. Seeing a handoff does not let you cause one.
- **`artifact.registered`** is first-class, not a sub-case of `workitem.*`: a produced
  build, diff, or blob emits its own event carrying the artifact `kind`.
- **`scm.check_run.failed`** is what a "re-run CI on failure" or "open a triage ticket"
  plugin keys off — remember the write-back goes through the public API, not the bus.

---

## 5. Plugin SDK API

The SDK is thin by design: the platform's contract with a plugin is **a NATS
subscription plus a typed catalog**. Everything else is your program.

### 5.1 Registration

A plugin registers against one or more Project/squads. Registration tells the platform
which squads' NATS credentials the plugin may use and pins the event-catalog rev the
plugin was built against. It does **not** grant any coordination capability — there is
none to grant.

### 5.2 NATS subscription

Use JetStream with a **durable consumer** for anything you can't afford to miss:

```go
js, _ := jetstream.New(nc)

// A durable consumer remembers its position across restarts.
cons, _ := js.CreateOrUpdateConsumer(ctx, "KSQUAD_EVENTS", jetstream.ConsumerConfig{
	Durable:       "acme-slack-notifier",       // stable name = resume, not restart
	FilterSubject: "ksquad.run.acme.>",
	AckPolicy:     jetstream.AckExplicitPolicy,  // ack only after your side effect succeeds
	DeliverPolicy: jetstream.DeliverAllPolicy,   // or DeliverNew to skip history
})

cons.Consume(func(msg jetstream.Msg) {
	if err := handle(msg.Data()); err != nil {
		msg.Nak() // negative-ack: JetStream will redeliver
		return
	}
	msg.Ack() // acknowledge only on success
})
```

- **Explicit ack after the side effect**, so a crash mid-handler redelivers rather than
  drops.
- **`Nak()` on transient failure** to get redelivery; combine with a max-deliver limit
  and a dead-letter subject for poison messages.
- **Core NATS** (`nc.Subscribe`) is fine for best-effort, fire-and-forget plugins that
  don't need replay.

### 5.3 Configuration via CRD

A plugin is declared per Project/squad through a configuration CRD (a plugin-config
resource, `ksquad.io/v1alpha1`). The shape below is illustrative — it declares the
subjects to observe and the **BYO Secret** the plugin uses for its outbound calls:

```yaml
apiVersion: ksquad.io/v1alpha1
kind: Plugin                 # per Project/squad plugin registration
metadata:
  name: slack-notifier
  namespace: acme-backend    # the squad's namespace (tenancy boundary)
spec:
  project: acme
  squad: backend
  subjects:
    - ksquad.run.acme.backend.succeeded
    - ksquad.run.acme.backend.failed
  # BYO outbound credential — a per-Project/per-user Secret, NEVER a shared master cred.
  credentialsSecretRef:
    name: slack-webhook
    key: url
```

The credential is **yours**, scoped to this squad. The platform never hands a plugin a
shared master credential (the credential lock holds for plugins too).

### 5.4 Idempotency and error handling

Because delivery is **at-least-once**, handlers must be **idempotent**:

- **Dedupe on `eventId`.** Keep a short-lived seen-set (or a unique constraint in your
  sink) so a redelivered event is a no-op the second time.
- **Make side effects safe to repeat.** Prefer upserts over inserts; use idempotency
  keys when the downstream API supports them (Slack, Stripe, PagerDuty all do).
- **Fail loudly, ack late.** Ack only after the side effect commits. On a transient
  error, `Nak()` for redelivery; on a permanent error (bad payload), route to a
  dead-letter subject and ack so you don't wedge the consumer.
- **Pin your catalog rev and decode defensively.** Ignore event types and schema
  versions you don't handle; never assume a field you didn't pin exists.
- **Never treat the bus as a command channel.** Publishing to `ksquad.*` does nothing to
  the platform. To *act*, call the public API as an authenticated client.

---

## 6. Examples

Three shapes of plugin, from the platform's own first consumer to common integrations.

### 6.1 GRAIL memory-backend plugin (the reference consumer)

KSquad's memory is Postgres + pgvector — the source of truth. **GRAIL is the first
plugin consumer**: it subscribes to the memory-write subjects and streams them into an
external knowledge graph (via OTLP / SmartScape / DQL), giving you org-wide memory
analytics **without** changing where memory actually lives.

```go
// Subscribe to every memory write across all projects, stream to GRAIL.
cons, _ := js.CreateOrUpdateConsumer(ctx, "KSQUAD_EVENTS", jetstream.ConsumerConfig{
	Durable:       "grail-memory-fanout",
	FilterSubject: "ksquad.memory.*.*.written",
	AckPolicy:     jetstream.AckExplicitPolicy,
})
cons.Consume(func(msg jetstream.Msg) {
	ev, err := events.Decode[events.MemoryWritten](msg.Data())
	if err != nil { msg.Ack(); return } // not ours
	if err := grail.Stream(ctx, ev); err != nil { msg.Nak(); return }
	msg.Ack()
})
```

This is the canonical pattern: **pgvector stays authoritative; the plugin is a
downstream fan-out**, never a backend swap. It cannot corrupt or override platform
memory — it only reads the write stream.

### 6.2 Slack notifier

React to terminal run outcomes and post to a channel. The Slack webhook is a BYO Secret
([§5.3](#53-configuration-via-crd)); posting is an ordinary outbound HTTP call.

```go
cons, _ := js.CreateOrUpdateConsumer(ctx, "KSQUAD_EVENTS", jetstream.ConsumerConfig{
	Durable:       "acme-slack-notifier",
	FilterSubject: "ksquad.run.acme.>",
	AckPolicy:     jetstream.AckExplicitPolicy,
})
cons.Consume(func(msg jetstream.Msg) {
	switch {
	case isType(msg, "run.succeeded"):
		ev, _ := events.Decode[events.RunSucceeded](msg.Data())
		postSlack(webhookURL, fmt.Sprintf("✅ run %s succeeded", ev.RunID))
	case isType(msg, "run.failed"):
		ev, _ := events.Decode[events.RunFailed](msg.Data())
		postSlack(webhookURL, fmt.Sprintf("❌ run %s failed: %s", ev.RunID, ev.Reason))
	}
	msg.Ack()
})
```

### 6.3 Metrics exporter

Turn the event stream into Prometheus counters. Subscribe broadly, increment labelled
counters, expose `/metrics`.

```go
var runsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{Name: "ksquad_runs_total"},
	[]string{"project", "squad", "outcome"},
)

cons.Consume(func(msg jetstream.Msg) {
	if ev, err := events.Decode[events.RunSucceeded](msg.Data()); err == nil {
		runsTotal.WithLabelValues(ev.Project, ev.Squad, "succeeded").Inc()
	} else if ev, err := events.Decode[events.RunFailed](msg.Data()); err == nil {
		runsTotal.WithLabelValues(ev.Project, ev.Squad, "failed").Inc()
	}
	msg.Ack()
})
```

> KSquad is already OTel-native, so much of this exists on the platform side. A metrics
> exporter plugin is for pushing squad activity into a metrics system you *already
> operate* on your own terms.

---

## 7. Testing plugins

You do not need a full KSquad install to develop a plugin. You need a NATS bus and some
events.

### 7.1 Local dev workflow

Run a local JetStream-enabled NATS:

```bash
# Docker
docker run --rm -p 4222:4222 nats:latest -js

# or the binary
nats-server -js
```

Point your plugin at `nats://127.0.0.1:4222`, then iterate with `go run ./observer`.

### 7.2 Mock the event bus

Replay realistic events without a running control plane by publishing fixtures with the
`nats` CLI — this is your "mock event bus":

```bash
# Publish a fixture run.succeeded to the exact subject your plugin subscribes to.
nats pub "ksquad.run.acme.backend.succeeded" \
  '{"eventId":"evt-123","type":"run.succeeded@v1","runId":"run-42","project":"acme","squad":"backend","phase":"Succeeded","initiatedBy":"alice","occurredAt":"2026-08-14T10:00:00Z"}'
```

Keep a directory of JSON fixtures — one per event type — and a small script that
publishes them in sequence to exercise every branch of your handler.

### 7.3 Unit-test the handler directly

Your side-effect logic shouldn't need NATS at all. Keep the decode/react function pure
and feed it raw bytes:

```go
func TestHandleRunSucceeded(t *testing.T) {
	data := []byte(`{"eventId":"e1","type":"run.succeeded@v1","runId":"run-42",...}`)
	got, err := handle(data)
	require.NoError(t, err)
	require.Equal(t, "run-42", got.RunID)
}
```

### 7.4 Verify idempotency and replay

- **Send the same event twice** and assert exactly one side effect (dedupe on `eventId`).
- **Restart your consumer mid-stream** and assert it resumes from the last ack (durable
  consumer), not from zero or from the tail.
- **Nak a poison message** and assert it lands in your dead-letter path after the
  max-deliver limit, without wedging the consumer.

---

## 8. Publishing

Plugins are ordinary programs — package and distribute them like any service.

### 8.1 Packaging

- **Container image.** Ship your plugin as an OCI image. Run it as a **sidecar** to the
  squad it observes, or as a **standalone Deployment** — both are supported topologies.
- **Configuration.** Ship the plugin-config CRD manifest ([§5.3](#53-configuration-via-crd))
  and document the Secret shape the operator must create (the BYO credential). Never bake
  credentials into the image.
- **Pin the catalog rev.** State which `pkg/events` catalog revision your plugin builds
  against, so operators know the compatibility window.

### 8.2 Distribution

- **Helm chart** (recommended) — bundle the Deployment/sidecar, the plugin-config CRD,
  and a values-driven Secret reference so operators install with one command.
- **Plain manifests** — a `plugin.yaml` + `deployment.yaml` for operators who don't use
  Helm.
- **Document, minimally:** which subjects it subscribes to, which events it reacts to,
  what external system it touches, and which Secret keys it needs. An operator should be
  able to reason about a plugin's blast radius from its README alone — and because
  plugins are read-only observers, that blast radius is *"it might be noisy or lag,"*
  never *"it might corrupt a squad."*

### 8.3 Compatibility & upgrades

The event catalog is **versioned with additive-or-gated drift discipline** — the same
pinned-adapter policy as the A2A and MCP surfaces. Producer changes are additive or
gated behind a new schema version; they are never ambient breakage. In practice:

- Decode defensively; ignore unknown fields and unhandled types.
- Pin the catalog rev you built against; upgrade deliberately.
- A third-party plugin built today keeps working across platform upgrades **as long as
  it pins its rev** — that is the whole point of the versioned catalog.

---

## References

- **Architecture §17.4 — Plugin Architecture & Event Seam** (*Postgres stores, NATS
  flows, plugins observe*). The authoritative design.
- **ADR-023 — Postgres source-of-truth + NATS/JetStream event bus.** The locked decision
  ("store the data in Postgres, flow the events on NATS"): durable outbox capture, relay
  + reconciliation, at-least-once delivery, plugins as read-only observers. Rejects
  outbox-as-plugin-API, Kafka, and pure in-process delivery.
- **ADR-024 — Memory-backend pluggability.** pgvector is source-of-truth; GRAIL is the
  seam's first consumer, subscribing to memory-write subjects.
- **Architecture §6.6 — coordination event marker;** **§7.3 / §7.5 — the no-P2P
  discipline** applied to memory and the discussion room (and, a third time, to plugins).
- **Architecture §10.2 — versioned event catalog / drift discipline** (`pkg/events@rev`).
- **Story 9.4 — NATS/JetStream subchart + apiserver outbox relay.** How the bus ships in
  the Helm chart (single-replica default, JetStream PVC, HA toggle; the relay never gates
  apiserver health).
- **Epic 12 — event catalog + relay/reconciliation worker + `pkg/events`.** Where the
  typed catalog and relay land.
- [`CONTRIBUTING.md`](../CONTRIBUTING.md) — building and testing KSquad from source.

> Found something in this guide that's wrong or has drifted from the code? Opening a PR
> to fix it is a great first contribution.
