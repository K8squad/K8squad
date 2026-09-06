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
	logglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// ============================================================================
// NFR-2 secret hygiene over TELEMETRY OUTPUT for the two TEST-CONNECTION paths
// (ISI-3882, the ISI-3669 remainder — obs plan §5.3). The create-path sweep
// (TestNFR2SecretNeverInTelemetry) proved the funnel module never leaks the
// value on POST /api/credentials; these prove the SAME for the paths whose
// handlers landed later:
//
//	POST /api/credentials/{name}/test    → span ksquad.credential.test
//	POST /api/projects/repo-auth/test    → span ksquad.repo.auth.test
//
// Each drives its handler with the stored credential set to a sentinel and
// asserts the sentinel appears in NO exported span and NO exported log record.
// The repo path additionally smuggles a token in the URL userinfo to prove the
// §5.3 repo-URL redaction (redactRepoURL) keeps it out of both span and log.
// Positive controls (the funnel span exists and carries a real outcome) keep
// the sweeps from passing vacuously on an empty capture.
// ============================================================================

// telemetryCapture wires an in-memory span exporter and an otelslog log
// bridge, returning readers over the serialized span and log output. It
// mirrors the setup TestNFR2SecretNeverInTelemetry does inline, so the two
// test-path sweeps share one honest capture.
func telemetryCapture(t *testing.T) (spans func() string, logs func() string) {
	t.Helper()

	spanExp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(spanExp)))
	prevTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)

	var logBuf bytes.Buffer
	logExp, err := stdoutlog.New(stdoutlog.WithWriter(&logBuf))
	if err != nil {
		t.Fatalf("stdoutlog: %v", err)
	}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logExp)))
	logglobal.SetLoggerProvider(lp)
	prevLogger := slog.Default()
	slog.SetDefault(otelslog.NewLogger("test", otelslog.WithLoggerProvider(lp)))

	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		slog.SetDefault(prevLogger)
		_ = lp.Shutdown(t.Context())
		_ = tp.Shutdown(t.Context())
	})

	spans = func() string {
		var b strings.Builder
		for _, s := range spanExp.GetSpans() {
			b.WriteString(s.Name)
			fmt.Fprint(&b, s.Attributes)
			fmt.Fprint(&b, s.Events)
			fmt.Fprint(&b, s.Status)
		}
		return b.String()
	}
	logs = func() string { return logBuf.String() }
	return spans, logs
}

func TestNFR2CredentialTestSecretNeverInTelemetry(t *testing.T) {
	spans, logs := telemetryCapture(t)

	// Stored material IS the sentinel; the probe reads it (proving the right
	// Secret was resolved), so if any funnel span attribute or log line echoed
	// the material this sweep would catch it.
	teamID := uuid.MustParse("21111111-2222-3333-4444-555555555555")
	tm := teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha")
	cred := managedCredential("ksquad-team-alpha", "alice-anthropic", "apiKey", secretValueCanary)
	svc, _, _ := newCredentialTester(t, tm, cred)
	h := testCredentialTestServer(t, teamID, svc)

	rec := postCredentialTest(t, h, "alice-anthropic", `{"runtime":"claude-code"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	sp, lg := spans(), logs()

	// Positive control: the funnel span exists and recorded a real outcome —
	// otherwise the sweep below would pass on an empty capture.
	if !strings.Contains(sp, "ksquad.credential.test") {
		t.Fatalf("positive control failed: no ksquad.credential.test span captured (spans: %s)", sp)
	}
	if !strings.Contains(sp, "passed") {
		t.Fatalf("positive control failed: outcome=passed not on the funnel span (spans: %s)", sp)
	}

	if strings.Contains(sp, secretValueCanary) {
		t.Fatalf("NFR-2 breach: credential value leaked into SPAN output: %s", sp)
	}
	if strings.Contains(lg, secretValueCanary) {
		t.Fatalf("NFR-2 breach: credential value leaked into LOG output: %s", lg)
	}
}

// repoURLTokenCanary is a token smuggled into the repo URL userinfo — the
// §5.3 redaction case that a raw url in a span attribute or log would leak.
const repoURLTokenCanary = "ghp_URLCANARY-in-userinfo-must-never-leak"

func TestNFR2RepoAuthTestSecretNeverInTelemetry(t *testing.T) {
	spans, logs := telemetryCapture(t)

	tm, sec, teamID := repoTestTeamAndSecret(t) // stored token == repoPatCanary
	svc, _, _ := newRepoAuthTester(t, tm, sec)
	h := testRepoAuthServer(t, teamID, svc)

	// The URL carries a token in its userinfo (§5.3): redactRepoURL must strip
	// it before it reaches the span attribute or the log line.
	body := `{"url":"https://` + repoURLTokenCanary + `@github.com/acme/widget","credentialSecretRef":{"name":"alpha-repo-pat"}}`
	rec := postRepoAuthTest(t, h, body, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	sp, lg := spans(), logs()

	// Positive control: the funnel span exists, recorded a real outcome, and
	// carries the REDACTED repo URL (host+path, no userinfo).
	if !strings.Contains(sp, "ksquad.repo.auth.test") {
		t.Fatalf("positive control failed: no ksquad.repo.auth.test span captured (spans: %s)", sp)
	}
	if !strings.Contains(sp, "passed") {
		t.Fatalf("positive control failed: outcome=passed not on the funnel span (spans: %s)", sp)
	}
	if !strings.Contains(sp, "github.com/acme/widget") {
		t.Fatalf("positive control failed: redacted repo URL not on the funnel span (spans: %s)", sp)
	}

	// The stored PAT never rides telemetry.
	if strings.Contains(sp, repoPatCanary) {
		t.Fatalf("NFR-2 breach: stored token leaked into SPAN output: %s", sp)
	}
	if strings.Contains(lg, repoPatCanary) {
		t.Fatalf("NFR-2 breach: stored token leaked into LOG output: %s", lg)
	}
	// The URL-embedded token never rides telemetry (§5.3 redaction).
	if strings.Contains(sp, repoURLTokenCanary) {
		t.Fatalf("§5.3 breach: URL userinfo token leaked into SPAN output: %s", sp)
	}
	if strings.Contains(lg, repoURLTokenCanary) {
		t.Fatalf("§5.3 breach: URL userinfo token leaked into LOG output: %s", lg)
	}
}
