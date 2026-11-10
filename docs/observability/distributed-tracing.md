# Observability: Distributed Tracing

KSquad is OpenTelemetry-first, and a dispatched run is traceable *as one trace* across every hop:
dispatch → gateway → mesh child → runtime events.

## W3C trace propagation end to end

Trace context propagates as W3C trace context headers from the control plane's dispatch leg, through
the gateway, into the mesh child that runs the work. A mid-run model switch or a rebind-at-wake does
not break the trace spine — the same run keeps the same trace identity across the switch.

## Ticket identity on every span

Spans carry `ksquad.ticket.ref`: the **human-readable ticket key** (an external board key, or a
GitHub `owner/repo#N` ref) resolved from the work item's labels and carried on the submit payload.
It is present only when a key actually resolved — never fabricated — and it lives on **span
attributes only, never as a metric label** (NFR-OBS3), so high-cardinality ticket keys cannot explode
metric series.

`ksquad.ticket.ref` is the join key between telemetry and the board: query by ticket, land on the
trace.

## Skill loads are spans too

`skill.load` spans record every skill an agent loads during a run, with span-identity detail on
`run.end` and gateway spans — so "which skills fired, in which run, for which ticket" is a trace
query, not a log scavenger hunt.

## Semantic conventions

The full attribute tables — stability, required/conditional status, and the NFR-OBS3 cardinality
rules — live in the [semantic conventions](./semantic-conventions.md) reference. Ready-made
dashboards (agent run drilldown, credential vault audit, run summary presence) ship alongside them
in this directory.

## Related

- [AgentMesh architecture](../agent-mesh.md)
- [Mid-run model fallback](../mid-run-model-fallback.md)
