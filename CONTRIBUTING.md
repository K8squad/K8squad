# Contributing to KSquad

Thanks for your interest in contributing! KSquad is a Kubernetes-native,
agent-agnostic AI agent orchestration platform. Contributions of all kinds —
code, docs, tests, issue triage — are welcome.

> Status: early development. Interfaces and CRDs are not yet stable, so expect
> churn and discuss substantial changes in an issue before you start.

## Code of Conduct

All participation is governed by our [Code of Conduct](CODE_OF_CONDUCT.md),
which adopts the CNCF Community Code of Conduct. By participating you are
expected to uphold it.

## Developer Certificate of Origin (DCO)

Every commit must be **signed off** to certify the
[Developer Certificate of Origin](DCO.md). Commit with `-s`:

    git commit -s -m "feat: add squad reconciler"

The `Signed-off-by` trailer must match your Git author identity. The DCO check
(`.github/workflows/dco.yml`) is a required status check on every pull request.
If you forget, fix it with `git rebase --signoff <base>` and force-push.

## Licensing

KSquad is licensed under the [Apache License 2.0](LICENSE). All contributions
are accepted under that license. If you add a dependency, record it in
[LICENSES-third-party](LICENSES-third-party) and make sure its license is
compatible (Apache-2.0, MIT, BSD, and similar permissive licenses are fine;
avoid copyleft without maintainer discussion).

## Development workflow

1. Fork the repository and create a topic branch off `main`.
2. Make your change with tests. See the `Makefile` for common targets and
   `.github/workflows/` for the checks that run in CI (lint, build, unit +
   integration tests, coverage, security scans).
3. Ensure the relevant checks pass locally where practical.
4. Sign off every commit (DCO) and open a pull request against `main`.
5. Fill in the PR description: what changed and why. Link the issue it resolves.

### Quality gates

- **Coverage:** ≥ 80% per Go package; the coordination-spine package is held to
  ≥ 90%.
- **Coordination spine:** changes to coordination code must pass the
  `spine-chaos.yml` concurrency/chaos suite — it is the most correctness-critical
  gate.
- Keep public CRD schema and security-relevant changes small and well-described;
  they require maintainer review.

## Reporting bugs and requesting features

Open a GitHub issue with clear reproduction steps or a concrete use case. For
**security vulnerabilities, do not open a public issue** — follow
[SECURITY.md](SECURITY.md).

## Governance

Project roles and decision-making are described in [GOVERNANCE.md](GOVERNANCE.md).
Current maintainers are listed in [MAINTAINERS.md](MAINTAINERS.md).
