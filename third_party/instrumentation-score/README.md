# third_party/instrumentation-score — vendored instrumentation-score rules

This directory vendors the rule set of the
[instrumentation-score/spec](https://github.com/instrumentation-score/spec)
project so K8squad's conformance scoring consumes a fixed, reviewable copy
with no network dependency at build or test time.

## Provenance

- Source: https://github.com/instrumentation-score/spec
- Pinned commit: `ef080a8a42d2fe73a4f0ed8d64eda93bfe84adc7` (2026-10-30)
- Vendored: 2026-11-24 (ISI-7747, phase 1 of the instrumentation-score
  conformance plan on ISI-7741)
- Contents: the 26 rule markdown files under `rules/` (LOG-001..003,
  MET-001..008, RES-001..007+010, SDK-001, SPA-001..006), plus
  `specification.md` and the upstream `LICENSE` (Apache-2.0).
  The upstream `rules/_template.md` authoring template is not vendored.

## License

Apache License, Version 2.0 — see [LICENSE](./LICENSE). Attribution is
recorded in the repository root [NOTICE](../../NOTICE).

## Sync policy

The canonical rule set lives upstream; this copy is a pin, not a fork.
The Go registry in `pkg/telemetry/instscore` must list every vendored rule
(id, target, impact); the `rulesync` test there fails if the two drift
apart. To pick up upstream changes:

1. Re-vendor at the new commit (update the pin above).
2. Update the registry in `pkg/telemetry/instscore/rules.go` to match.
3. Run `go test ./pkg/telemetry/instscore/...` — it must pass with the new
   inventory, making any score-affecting drift visible in review.
