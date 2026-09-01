/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package e2e is the Run-path conformance harness (ISI-3475 / ISI-2114),
// the live end-to-end proof of the squad Run plane that the QA gate
// (ISI-3290 AC-2 + the in-Run half of AC-4) depends on.
//
// The actual harness — TestSquadSmoke and its helpers — is gated behind the
// `e2e` build tag (see the `//go:build e2e` files in this directory) so it is
// NEVER compiled or run by the untagged unit lane (`go test ./...`). This file
// carries no build tag so the package is always importable and `go build ./...`
// never trips "no Go files" on this directory.
//
// # What TestSquadSmoke proves
//
// On a kind cluster with the operator + default toolchain catalog installed,
// it drives ONE live Run that requires kubectl@1.31 + the github-mcp MCP server
// and asserts the five Run-path conformance properties from the ISI-3475
// deliverable:
//
//  1. kubectl @1.31 is on PATH inside the Run sandbox (version-pinned).
//  2. A SCOPED MCP config is rendered for github-mcp only — the single declared
//     server, secret-bearing headers injected from a Secret ref, never inlined.
//  3. Credentials are MOUNTED from the referenced Secret(s).
//  4. Egress is HONORED — an allowlisted upstream is reachable via the team
//     proxy while an arbitrary destination is refused (mirrors
//     test/blast-radius/cases/s4-1-egress-default-deny.sh).
//  5. tool / MCP / skill spans + metrics are emitted during the Run and visible
//     on the OTel sink (closes AC-4's in-Run half; mirrors the
//     internal/a2a/telemetrysink_e2e_test.go scrape pattern).
//
// # Skip-with-reason, never silently dropped
//
// Following the repo-wide L1/E2E convention (§3.3, §10.4, test/l1/README.md and
// e2e.yml's own skeleton gates), every precondition the harness cannot satisfy
// yet — no reachable cluster, the operator CRDs absent, the default toolchain
// catalog not installed, the Run never reaching a resolved phase — surfaces as
// a t.Skip with a precise reason rather than a silent pass or a hard failure.
// The harness thus goes green-and-runs incrementally as the operator + catalog
// land, and turns the e2e.yml `test/e2e-conformance` half of the skip-guard
// from missing to present the moment this directory exists.
package e2e
