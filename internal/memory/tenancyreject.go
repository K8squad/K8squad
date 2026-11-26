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
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// tenancyreject.go — operator-visible signal for a memory-edge tenancy
// rejection (ISI-7873, WS-A of ISI-7868). A warm-pool discussion/party run that
// reaches a memory MCP edge without a resolvable caller tenant is rejected with
// a 401 "X-Team-Id required (server-authenticated caller tenant)" — and until
// now that rejection was SILENT at the operator layer: the agent was blinded
// (no memory_search / work_item_search / diary) while /metrics and the logs
// showed nothing, so a recurrence could not alert. This makes every rejection
// visible WITHOUT changing a byte of the auth decision (the 401 still fires
// exactly as before; this only records that it did).
//
// The instrument is BOUNDED — one counter over a fixed {transport, reason}
// enum, NO per-team / per-run / per-principal labels — the same NFR-OBS3
// cardinality firewall recallhealth and the coord/event-seam metrics obey (the
// authoritative per-request record is the structured log below, never a metric
// label):
//
//	ksquad_memory_tenancy_rejected_total  counter{transport,reason}
//
// Until RegisterTenancyMetrics is called the counter is a no-op, so the two
// transports (toolhttp.go, toolmcp.go) can emit unconditionally and tests /
// binaries that never register stay silent.

// Transport labels — the memory edge a rejection fired on.
const (
	// TenancyTransportHTTP is the thin per-tool JSON/HTTP compat shim (toolhttp.go,
	// /mcp/tools/*) — the surface that returns a real HTTP 401 on a missing tenant.
	TenancyTransportHTTP = "http"
	// TenancyTransportMCP is the MCP JSON-RPC transport (toolmcp.go, /mcp) — a
	// missing tenant surfaces there as a tools/call isError, the JSON-RPC analogue
	// of the HTTP 401.
	TenancyTransportMCP = "mcp"
)

// Rejection reasons — the closed label set. Emitters MUST use these constants;
// adding a value is a deliberate, reviewed act (NFR-OBS3).
const (
	// TenancyReasonMissingTeam: no server-authenticated tenant resolved — neither
	// a verified run-capability-token claim nor an X-Team-Id header was present.
	// This is the warm-pool silent-401 of ISI-7868: the run bearer never reached
	// the edge, so the agent is blinded.
	TenancyReasonMissingTeam = "missing_team"
	// TenancyReasonMissingPrincipal: a write/author tool resolved a tenant but no
	// server-authenticated author (no principal) — the write 401 peer.
	TenancyReasonMissingPrincipal = "missing_principal"
	// TenancyReasonBearerVerifyFailed: a bearer run-capability token was presented
	// but failed verification. The edge fails closed to 401 and never falls back to
	// header trust (a junk token paired with forged X-* headers must not pass).
	TenancyReasonBearerVerifyFailed = "bearer_verify_failed"
)

var (
	tenancyWired     atomic.Bool
	tenancyRegOnce   sync.Once
	tenancyRejectCtr = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ksquad_memory_tenancy_rejected_total",
		Help: "Memory-edge tenancy rejections (401 / tools-call isError) by transport and reason. A non-zero rate is a warm-pool run reaching a memory MCP edge without a resolvable caller tenant (ISI-7868) — an agent blinded to memory/search/diary.",
	}, []string{"transport", "reason"})
)

// RegisterTenancyMetrics wires the tenancy-rejection counter on reg (ksquad-memory
// passes its own registry, served at /metrics on the health port — the same one
// recallhealth.Register uses). Idempotent; the first call wins. Until it is called
// the counter is a no-op.
func RegisterTenancyMetrics(reg prometheus.Registerer) {
	tenancyRegOnce.Do(func() {
		reg.MustRegister(tenancyRejectCtr)
		tenancyWired.Store(true)
	})
}

// TenancyRejected records one rejection: it ticks the bounded counter AND emits a
// structured warn log carrying the (bounded) transport/reason plus whether the
// request arrived on the token path — the operator-visible signal acceptance #2
// demands. It is safe to call before RegisterTenancyMetrics (the counter stays a
// no-op; the log still fires, so a rejection is never fully silent). No tenant,
// principal, or run id is logged: a missing-tenant rejection has none, and the
// bounded shape is the point.
func TenancyRejected(ctx context.Context, transport, reason string, viaToken bool) {
	if tenancyWired.Load() {
		tenancyRejectCtr.WithLabelValues(transport, reason).Inc()
	}
	slog.WarnContext(ctx, "memory edge tenancy rejection (ISI-7873): caller reached the edge without a resolvable server-authenticated tenant — the agent is blinded to this tool",
		"transport", transport,
		"reason", reason,
		"viaToken", viaToken,
	)
}
