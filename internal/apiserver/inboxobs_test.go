package apiserver

// inboxobs_test.go — ISI-5621 / ISI-5539 WS-G G-1(+G-4): the Inbox aggregate-read
// instrumentation. Asserts the two signals that ADR-0026 §3.2/§9 asked to make
// visible: a silently-degrading arm emits an inbox.arm_degraded span event (with
// an error CLASS, never the message — PII posture), and an arm that hits its
// LIMIT cap emits inbox.arm_truncated so a fleet admin's clipped view is not
// silent. Reuses telemetryCapture from obsfunnel_testpaths_test.go.

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// TestInboxArmDegradeEmitsSignal — a review arm that errors must surface as an
// inbox.arm_degraded span event carrying the bounded error class, and the raw
// error message must NOT appear anywhere on the span (PII posture).
func TestInboxArmDegradeEmitsSignal(t *testing.T) {
	spans, _ := telemetryCapture(t)

	team := uuid.New()
	req := inboxReq("alice", team)
	rec := httptest.NewRecorder()

	reviews := fakeReviews{err: errors.New("pg-connection-refused-secret-dsn")}
	srv := &Server{}
	srv.squadInbox(reviews, fakeProposals{}, fakeDecisions{}, fakeMarkers{}, fakeOverview{}, nil).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("squadInbox: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	out := spans()
	if !strings.Contains(out, "inbox.read") {
		t.Errorf("missing inbox.read span; got %q", out)
	}
	if !strings.Contains(out, "inbox.arm_degraded") {
		t.Errorf("missing inbox.arm_degraded event for the erroring review arm; got %q", out)
	}
	if !strings.Contains(out, "error.type") {
		t.Errorf("degrade event missing bounded error.type class; got %q", out)
	}
	// PII posture: the raw error message (a stand-in for a DSN/secret) must never ride the span.
	if strings.Contains(out, "pg-connection-refused-secret-dsn") {
		t.Errorf("raw arm error message leaked onto the span (PII posture violated); got %q", out)
	}
}

// TestInboxArmTruncationEmitsSignal — a proposal arm returning exactly the LIMIT
// cap of rows must flag truncation (span event + attribute), the G-4 no-silent-
// truncation signal.
func TestInboxArmTruncationEmitsSignal(t *testing.T) {
	spans, _ := telemetryCapture(t)

	proposals := fakeProposals{items: make([]discussion.OpenProposalSummary, inboxArmCap)}
	for i := range proposals.items {
		proposals.items[i] = discussion.OpenProposalSummary{
			MessageID: fmt.Sprintf("m-%d", i),
			CreatedAt: time.Now(),
		}
	}

	team := uuid.New()
	req := inboxReq("alice", team)
	rec := httptest.NewRecorder()

	srv := &Server{}
	srv.squadInbox(fakeReviews{}, proposals, fakeDecisions{}, fakeMarkers{}, fakeOverview{}, nil).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("squadInbox: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	out := spans()
	if !strings.Contains(out, "inbox.arm_truncated") {
		t.Errorf("proposal arm at LIMIT cap (%d) did not emit inbox.arm_truncated; got %q", inboxArmCap, out)
	}
	if !strings.Contains(out, "ksquad.inbox.arm.proposal.truncated") {
		t.Errorf("missing per-arm truncated span attribute; got %q", out)
	}
}
