# Security Policy

The KSquad maintainers take the security of the project seriously. Thank you for
helping keep KSquad and its users safe.

## Supported versions

KSquad is in early development and has not yet cut a stable release. Until a
`v1.0.0` release is tagged, only the `main` branch (and the most recent tagged
pre-release, if any) receives security fixes.

| Version | Supported |
|---------|-----------|
| `main` (unreleased) | ✅ |
| Tagged `v0.x` pre-releases | ✅ latest only |

Once stable releases exist, this table will be updated with the supported
release lines.

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues,
pull requests, or discussions.**

Report privately through one of:

1. **GitHub Security Advisories** (preferred) — open a private report via the
   repository's **Security → Report a vulnerability** page:
   https://github.com/K8squad/K8squad/security/advisories/new
2. **Email** — send details to the maintainers at
   <security@ksquad.io>. Encrypt with the maintainers' PGP key if you handle
   sensitive details (key fingerprint to be published with the first release).

Please include, as far as you can:

- A description of the vulnerability and its impact.
- Steps to reproduce or a proof of concept.
- Affected component(s), version(s), commit SHA, and configuration.
- Any suggested remediation.

## Our commitment

- **Acknowledgement** within **3 business days** of your report.
- An initial **assessment and severity triage** within **10 business days**.
- Regular updates on remediation progress, at least every **10 business days**
  until resolution.
- **Coordinated disclosure:** we will agree on a disclosure timeline with you.
  Our target is to release a fix within **90 days** of triage, sooner for
  actively exploited issues. We will credit reporters who wish to be named.

## Scope

In scope: the KSquad operator, coordination spine, API server, memory service,
console, and the runtime shims in this repository.

Out of scope: vulnerabilities in third-party dependencies (report those
upstream; we will pick up fixed versions), findings that require privileged
access already equivalent to the reported impact, and issues in
example/experimental code clearly marked as such.

## Safe harbor

We consider security research and vulnerability disclosure conducted in good
faith and in accordance with this policy to be authorized. We will not pursue or
support legal action against researchers who follow this policy.
