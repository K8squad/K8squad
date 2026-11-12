# Mid-Run Model Fallback

A rate limit used to mean a dead run. KSquad now handles rate-limit verdicts from model endpoints
with two possible outcomes, decided *while the run is still live*:

- **SwitchModel** — the **same Run continues** on a configured fallback model;
- **PauseRun** — the run pauses cleanly when no fallback is configured, instead of burning retries.

## How it works

When a model endpoint reports a rate-limit condition, the dispatch leg's decision core (the
"5.11 verdict") picks between the two outcomes. The verdict is handed to a switch executor that
rebinds the live run to the fallback model — the run's identity, checkout, and coordination record
stay intact. Only the model changes.

If no fallback model resolves for the agent, the verdict degrades to `PauseRun`: the run is paused
and can resume once the rate limit clears, rather than failing.

## Configuring a fallback

The default-tier fallback is set at install/upgrade time on the control-plane chart:

```yaml
modelConfig:
  default:
    model: claude-sonnet-4          # REQUIRED — no default; install fails if unset
    fallbackModel:                  # optional rate-limit secondary
      model: claude-haiku-4
```

The fallback is resolved per run from the agent/model configuration chain; the default tier is the
last resort of resolution, so a fallback there covers every agent that does not pin its own.

## What to expect on the board

A run that switched models keeps its run summary and trace spine; the switch is visible in telemetry
rather than as a new run. See [distributed tracing](./observability/distributed-tracing.md) for
following a single run's trace across the switch.
