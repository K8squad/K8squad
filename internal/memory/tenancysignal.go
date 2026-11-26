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

package memory

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
)

// tenancysignal.go — the operator-visible signal for tenancy rejections at
// the memory tool edges (ISI-7873, parent ISI-7868 work item 3). Until now a
// tenancy 401 was silent at the HTTP layer: the caller saw the rejection,
// but nothing on the memory service told an operator that agent calls were
// being blinded — a warm-pool discussion/party run whose rendered MCP config
// failed to carry the run bearer token simply lost its tool surface with no
// trace beyond per-request 401s. This file makes every such rejection
// visible: one structured log line plus one bounded Prometheus counter tick.
// It is observability only — no behavior change to the rejection itself.
//
// The counter is BOUNDED (NFR-OBS3): surface (http|mcp) + tool (the fixed
// tool-name set) + reason (the closed set below) — never per-team, per-run
// or per-principal labels; the authoritative record for a specific rejection
// is the caller's own error response, never a metric.
var tenancyRejections = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "ksquad_memory_tenancy_rejections_total",
		Help: "Tool-edge calls the memory service rejected because the caller could not be authenticated to a tenant: a missing server-authenticated identity on the trusted BFF header path, or an invalid run capability token on the sandbox bearer path (ISI-7873). Bounded labels: surface, tool, reason.",
	},
	[]string{"surface", "tool", "reason"},
)

// Tenancy rejection reasons — the closed label set (NFR-OBS3). Adding a
// value is a deliberate, reviewed act.
const (
	// tenancyReasonMissingTeam: neither a verified token claim nor the
	// X-Team-Id BFF header supplied the caller's tenant.
	tenancyReasonMissingTeam = "missing_team"
	// tenancyReasonMissingPrincipal: a write tool caller had tenancy but no
	// authenticated author.
	tenancyReasonMissingPrincipal = "missing_principal"
	// tenancyReasonBadBearer: a presented run capability token failed
	// verification (fail-closed; no header fallback).
	tenancyReasonBadBearer = "bad_bearer"
)

// RegisterTenancySignal registers the tenancy-rejection instruments on the
// memory service's own registry (cmd/memory wires its promReg — the default
// registerer stays untouched so tests and library use stay silent). Until
// registered the counter is inert; the structured log always fires.
func RegisterTenancySignal(reg prometheus.Registerer) {
	if reg == nil {
		return
	}
	reg.MustRegister(tenancyRejections)
}

// tenancyRejection emits the operator signal for one rejected call: one
// bounded counter tick plus one structured warn line carrying the
// surface/tool/reason triple — never a token, header value or body.
func tenancyRejection(surface, tool, reason string) {
	tenancyRejections.WithLabelValues(surface, tool, reason).Inc()
	slog.Warn("memory: tool-edge tenancy rejection — caller not authenticated to a tenant (ISI-7873 signal)",
		"surface", surface, "tool", tool, "reason", reason)
}
