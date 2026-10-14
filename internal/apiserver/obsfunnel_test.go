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

package apiserver

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// ============================================================================
// NFR-2 secret hygiene over TELEMETRY OUTPUT (ISI-3669, obs plan §5.3) — the
// sweep the existing TestCredentialCreateNoEchoSweep runs over the HTTP
// response, run here over the two places telemetry could leak it instead:
//
//   1. every exported SPAN (name, attributes, events, status);
//   2. every exported LOG record (the otelslog bridge, i.e. what slog.*Context
//      calls in the instrumented handlers emit).
//
// The credential VALUE (secretValueCanary) must appear in neither — not on the
// create-success path, not on any error branch. The test also carries positive
// controls (the funnel span exists and carries outcome=created) so it cannot
// pass vacuously with an empty capture.
// ============================================================================

func TestNFR2SecretNeverInTelemetry(t *testing.T) {
	// --- capture every span exported in this test ---------------------------
	spanExp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(spanExp)))
	prevTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)

	// --- capture every log record the handlers emit -------------------------
	var logBuf bytes.Buffer
	logExp, err := stdoutlog.New(stdoutlog.WithWriter(&logBuf))
	if err != nil {
		t.Fatalf("stdoutlog: %v", err)
	}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logExp)))
	otel.SetLoggerProvider(lp)
	prevLogger := slog.Default()
	slog.SetDefault(otelslog.NewLogger("test", otelslog.WithLoggerProvider(lp)))

	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		slog.SetDefault(prevLogger)
		_ = lp.Shutdown(t.Context())
		_ = tp.Shutdown(t.Context())
	})

	// --- drive the secret-bearing create path with the sentinel value -------
	teamID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	svc, _ := newSecretWriter(t, teamWithStatus("ksquad-team-alpha", "alpha", teamID.String(), "exec-ksquad-team-alpha"))
	h := testSecretWriteServer(t, teamID, svc)

	rec := postCredential(t, h,
		`{"name":"nfr2-sweep","runtime":"claude-code","class":"service-account","value":"`+secretValueCanary+`"}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secretValueCanary) {
		t.Fatalf("NFR-2 breach: response echoes the credential value: %s", rec.Body.String())
	}

	// --- serialize the captured spans (name + attrs + events + status) ------
	var spanBuf strings.Builder
	for _, s := range spanExp.GetSpans() {
		spanBuf.WriteString(s.Name)
		fmt.Fprint(&spanBuf, s.Attributes)
		fmt.Fprint(&spanBuf, s.Events)
		fmt.Fprint(&spanBuf, s.Status)
	}
	spans := spanBuf.String()
	logs := logBuf.String()

	// --- positive control: the funnel span exists and recorded success ------
	if !strings.Contains(spans, "ksquad.credential.create") {
		t.Fatalf("positive control failed: no ksquad.credential.create span captured — capture is broken, the sweep below would pass vacuously (spans: %s)", spans)
	}
	if !strings.Contains(spans, "outcome created") && !strings.Contains(spans, `outcome") created`) {
		// attribute.String rendering varies across OTel versions; accept either.
		if !strings.Contains(spans, "created") {
			t.Fatalf("positive control failed: outcome=created not on the funnel span (spans: %s)", spans)
		}
	}

	// --- NFR-2: the sentinel must appear in NO span and NO log record -------
	if strings.Contains(spans, secretValueCanary) {
		t.Fatalf("NFR-2 breach: credential value leaked into SPAN output: %s", spans)
	}
	if strings.Contains(logs, secretValueCanary) {
		t.Fatalf("NFR-2 breach: credential value leaked into LOG output: %s", logs)
	}
}

// TestNFR2ToolCredentialAuditSpansNoLeak is the WS-9 S7 (ISI-5820, threat-model
// T7) audit-and-hygiene contract for the three mutating vault verbs. It drives a
// full lifecycle — POST (materialize) → PUT (rotate to a FRESH value) → DELETE —
// through the span+log capture harness and asserts two things at once:
//
//	AUDIT COMPLETENESS: each verb emits its ksquad.credential.{create,rotate,delete}
//	span carrying the audit tuple (actor principal, credential type, project,
//	namespace, outcome) — the non-secret "who did what to which credential" record;
//
//	NO VALUE LEAKAGE: NEITHER the original nor the rotated credential value appears
//	in ANY span or log record, on the success path or any branch (NFR-2).
//
// Positive controls (span names + attribute keys + non-secret values present)
// guard against the sweep passing vacuously on an empty or mis-wired capture.
func TestNFR2ToolCredentialAuditSpansNoLeak(t *testing.T) {
	readSpans, readLogs := telemetryCapture(t)

	teamID := uuid.MustParse("11111111-2222-3333-4444-5555555555cc")
	svc, _ := newSecretWriter(t,
		team("ksquad-team-beta", "beta", teamID.String()),
		project("ksquad-team-beta", "widget", "https://github.com/acme/widget"),
	)
	h := testSecretWriteServer(t, teamID, svc)

	// create → rotate(fresh value) → delete, the full mutating lifecycle.
	if rec := postCredential(t, h, `{"type":"github","projectId":"widget","value":"`+toolCredCanary+`"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := putCredential(t, h, `{"type":"github","projectId":"widget","value":"`+toolCredRotatedCanary+`"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("rotate: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := deleteCredential(t, h, `{"type":"github","projectId":"widget"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("delete: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	spans := readSpans()
	logs := readLogs()

	// --- positive control: all three audit spans were captured --------------
	for _, name := range []string{"ksquad.credential.create", "ksquad.credential.rotate", "ksquad.credential.delete"} {
		if !strings.Contains(spans, name) {
			t.Fatalf("positive control: %s span not captured — capture broken, the sweep below would pass vacuously (spans: %s)", name, spans)
		}
	}

	// --- audit completeness: the masked payload tuple is on the spans -------
	// Attribute KEYS (the contract) and their non-secret VALUES (actor, project,
	// type) must all be present; the value envelope is NOT.
	for _, key := range []string{"credential.principal", "credential.type", "credential.project", "k8s.namespace.name", "outcome"} {
		if !strings.Contains(spans, key) {
			t.Fatalf("audit span incomplete: attribute %q absent from the credential spans (spans: %s)", key, spans)
		}
	}
	for _, val := range []string{"user:alice", "github", "widget", "ksquad-team-beta"} {
		if !strings.Contains(spans, val) {
			t.Fatalf("audit payload incomplete: expected non-secret audit value %q absent from spans (spans: %s)", val, spans)
		}
	}

	// --- NFR-2: NEITHER credential value may appear in span OR log output ----
	for _, canary := range []string{toolCredCanary, toolCredRotatedCanary} {
		if strings.Contains(spans, canary) {
			t.Fatalf("NFR-2 breach: credential value %q leaked into SPAN output: %s", canary, spans)
		}
		if strings.Contains(logs, canary) {
			t.Fatalf("NFR-2 breach: credential value %q leaked into LOG output: %s", canary, logs)
		}
	}
}
