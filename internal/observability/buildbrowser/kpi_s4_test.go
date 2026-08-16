//go:build kpi

package buildbrowser_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Minimal interfaces for KPI-3 — wire real impls from ISI-2168 8.7d.
// ---------------------------------------------------------------------------

// HTTPClient is the BFF test client (Next.js BFF or Go integration shim).
type HTTPClient interface {
	GET(path string, opts ...RequestOpt) *http.Response
}

// RequestOpt modifies a test HTTP request.
type RequestOpt func(r *http.Request)

// ScopeDenialMetrics reads the scope.denied counter from the test metric sink.
type ScopeDenialMetrics interface {
	// ScopeDenied returns the count of scope.denied increments for the given endpoint.
	ScopeDenied(endpoint string) int64
	// Reset clears all counters (call before each sub-test).
	Reset()
}

// LogEntry is a captured structured log line.
type LogEntry interface {
	Field(key string) string
}

// LogSink captures log lines emitted during a test.
type LogSink interface {
	// LastWarnMatching returns the last WARN line where field=value, or nil.
	LastWarnMatching(field, value string) LogEntry
	Reset()
}

// Principal represents a KSquad tenant principal.
type Principal struct{ ID uuid.UUID }

func withPrincipal(p Principal) RequestOpt {
	return func(r *http.Request) {
		r.Header.Set("X-KSquad-Principal", p.ID.String())
	}
}

// bffEndpoints are all read surfaces that must be individually guarded.
var bffEndpoints = []string{"tree", "diff", "file", "meta"}

// ---------------------------------------------------------------------------
// KPI-3: S4 / NFR-SEC5 — cross-principal read emits scope.denied + WARN + 404.
// ---------------------------------------------------------------------------

// TestCrossPrincipalReadEmitsScopeDenied asserts the three-part S4 gate:
//  1. HTTP 404 (existence never confirmed in the response).
//  2. scope.denied counter increments.
//  3. WARN log carries id-only provenance; no path, no content.
//
// Pending: ISI-2168 8.7d (BFF + apiserver scoping instrumentation).
func TestCrossPrincipalReadEmitsScopeDenied(t *testing.T) {
	t.Skip("pending ISI-2168 8.7d — wire bff client, metrics sink, and log sink")

	// Replace with real implementations from Epic 8.7 fixtures.
	var bff     HTTPClient         // = bfftest.NewClient(t)
	var metrics ScopeDenialMetrics // = otel.NewTestSink()
	var logs    LogSink            // = logtest.NewSink(t)

	principalA := Principal{ID: uuid.New()}
	principalB := Principal{ID: uuid.New()} // cross-principal — must be denied

	// A run owned by principalA.
	var runID uuid.UUID // = fixtures.RunOwnedBy(t, principalA)
	_ = principalA      // used by fixtures.RunOwnedBy once ISI-2168 lands

	for _, ep := range bffEndpoints {
		t.Run(ep, func(t *testing.T) {
			metrics.Reset()
			logs.Reset()

			resp := bff.GET(
				fmt.Sprintf("/api/build/%s/%s", runID, ep),
				withPrincipal(principalB),
			)

			// 1. Response must be 404 — existence never confirmed.
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("endpoint=%s: got HTTP %d, want 404 (cross-principal read must not confirm existence)",
					ep, resp.StatusCode)
			}

			// 2. scope.denied counter must have incremented.
			if got := metrics.ScopeDenied(ep); got != 1 {
				t.Errorf("endpoint=%s: scope.denied = %d, want 1 — S4 metric missing", ep, got)
			}

			// 3. WARN log must carry id-only provenance.
			warn := logs.LastWarnMatching("outcome", "denied")
			if warn == nil {
				t.Fatalf("endpoint=%s: no WARN log emitted for scope.denied — S4 audit trail broken", ep)
			}
			if got := warn.Field("ksquad.run.id"); got != runID.String() {
				t.Errorf("endpoint=%s: WARN log ksquad.run.id = %q, want %s", ep, got, runID)
			}
			if got := warn.Field("ksquad.principal.id"); got != principalB.ID.String() {
				t.Errorf("endpoint=%s: WARN log ksquad.principal.id = %q, want %s", ep, got, principalB.ID)
			}
			if got := warn.Field("ksquad.buildbrowser.endpoint"); got != ep {
				t.Errorf("endpoint=%s: WARN log endpoint field = %q, want %s", ep, got, ep)
			}
			// Path must not appear — unbounded + sensitivity.
			if got := warn.Field("ksquad.buildbrowser.path"); got != "" {
				t.Errorf("endpoint=%s: WARN log must not carry path, got %q", ep, got)
			}
			// File content must never appear in any telemetry field.
			if got := warn.Field("body"); got != "" {
				t.Errorf("endpoint=%s: WARN log must not carry body content, got %q", ep, got)
			}
		})
	}
}

// TestCrossPrincipalReadDoesNotLeakExistenceInResponse asserts that a
// cross-principal read of an existing run and a request for a non-existent
// run are indistinguishable at the HTTP layer.
//
// The WARN log distinguishes them for S4 audit; the response never does.
//
// Pending: ISI-2168 8.7d.
func TestCrossPrincipalReadDoesNotLeakExistenceInResponse(t *testing.T) {
	t.Skip("pending ISI-2168 8.7d — wire bff client and metrics sink")

	var bff     HTTPClient
	var metrics ScopeDenialMetrics

	principalA := Principal{ID: uuid.New()}
	principalB := Principal{ID: uuid.New()}
	var runID uuid.UUID // = fixtures.RunOwnedBy(t, principalA)
	_ = principalA      // used by fixtures.RunOwnedBy once ISI-2168 lands
	nonExistentRunID := uuid.New()

	metrics.Reset()

	respOwned := bff.GET(
		fmt.Sprintf("/api/build/%s/tree", runID),
		withPrincipal(principalB),
	)
	respMissing := bff.GET(
		fmt.Sprintf("/api/build/%s/tree", nonExistentRunID),
		withPrincipal(principalB),
	)

	if respOwned.StatusCode != http.StatusNotFound {
		t.Errorf("existing-but-denied run: got HTTP %d, want 404", respOwned.StatusCode)
	}
	if respMissing.StatusCode != http.StatusNotFound {
		t.Errorf("non-existent run: got HTTP %d, want 404", respMissing.StatusCode)
	}

	// scope.denied fires only for the cross-principal case, not the not-found case.
	if got := metrics.ScopeDenied("tree"); got != 1 {
		t.Errorf("scope.denied = %d, want exactly 1 (only the cross-principal access, not the missing-run 404)", got)
	}
}
