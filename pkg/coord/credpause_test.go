// credpause_test.go — unit tests for the credential pause/resume contract that
// need NO Postgres (Stories 7.4+7.6 / ISI-2898): the reason/class taxonomies and
// their fail-closed guards, the timer-vs-refresh split, config validation, the
// pure re-route/scheduling advisories (SelectAlternate / EarliestResume), the
// shim event↔step↔reason mappings, and the fail-closed validation branches of
// PauseCredential / ApplyCredentialSignal that return before any DB work.
//
// The database-backed properties (durable resume_at, SKIP LOCKED exactly-once,
// refresh-mode clearing, the atomic ApplyCredentialSignal co-commit) live in
// credpause_chaos_test.go behind -tags=chaos, the same split resume.go uses.
package coord

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

// offlineConnector yields a *sql.DB whose connection is never established. The
// validation branches under test all return BEFORE BeginTx, so the handle is
// only ever checked for non-nil — this keeps the unit lane free of a live
// Postgres and independent of the pgx driver (registered only under -tags=chaos).
type offlineConnector struct{}

func (offlineConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("offline: no connection expected in the unit lane")
}
func (offlineConnector) Driver() driver.Driver { return offlineDriver{} }

type offlineDriver struct{}

func (offlineDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("offline")
}

// ---------------------------------------------------------------------------
// Reason + class taxonomies (fail-closed)
// ---------------------------------------------------------------------------

func TestCredPauseReasonValidAndTimerMode(t *testing.T) {
	timer := map[CredPauseReason]bool{ReasonRateLimited: true}
	for _, r := range []CredPauseReason{
		ReasonRateLimited, ReasonCredentialExpired, ReasonCredentialRotated, ReasonEndpointUnreachable,
	} {
		if !r.Valid() {
			t.Errorf("%q should be a valid reason", r)
		}
		if got := r.TimerResumed(); got != timer[r] {
			t.Errorf("%q.TimerResumed() = %v, want %v", r, got, timer[r])
		}
	}
	for _, r := range []CredPauseReason{"", "quota_exceeded", "rate-limited", "expired"} {
		if r.Valid() {
			t.Errorf("%q must not be a valid reason (fail-closed)", r)
		}
	}
}

func TestCredentialClassValid(t *testing.T) {
	for _, c := range []CredentialClass{ClassClaudeOAuth, ClassAPIKey, ClassBYOEndpoint} {
		if !c.Valid() {
			t.Errorf("%q should be a valid class", c)
		}
	}
	for _, c := range []CredentialClass{"", "kerberos", "oauth", "apikey"} {
		if c.Valid() {
			t.Errorf("%q must not be a valid class (fail-closed)", c)
		}
	}
}

// ---------------------------------------------------------------------------
// Config validation
// ---------------------------------------------------------------------------

func TestCredPauseConfigValidate(t *testing.T) {
	base := DefaultCredPauseConfig()
	base.Pause = "coord.credential_pause"
	if err := base.validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}

	cases := []struct {
		name string
		mut  func(c *CredPauseConfig)
	}{
		{"missing table", func(c *CredPauseConfig) { c.Pause = "" }},
		{"zero base", func(c *CredPauseConfig) { c.BackoffBase = 0 }},
		{"zero cap", func(c *CredPauseConfig) { c.BackoffCap = 0 }},
		{"zero reset", func(c *CredPauseConfig) { c.BackoffReset = 0 }},
		{"cap below base", func(c *CredPauseConfig) { c.BackoffBase = time.Minute; c.BackoffCap = time.Second }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultCredPauseConfig()
			c.Pause = "coord.credential_pause"
			tc.mut(&c)
			if err := c.validate(); err == nil {
				t.Fatalf("config %q must be rejected", tc.name)
			}
		})
	}
}

func TestBatchParamUnlimitedFallback(t *testing.T) {
	c := DefaultCredPauseConfig()
	c.ResumeBatch = 0
	if got := c.batchParam(); got <= 0 {
		t.Fatalf("ResumeBatch<=0 must map to an unlimited positive LIMIT, got %d", got)
	}
	c.ResumeBatch = 32
	if got := c.batchParam(); got != 32 {
		t.Fatalf("batchParam = %d, want 32", got)
	}
}

// ---------------------------------------------------------------------------
// Pure re-route + scheduling advisories (7.6 → 2.10)
// ---------------------------------------------------------------------------

func TestSelectAlternate(t *testing.T) {
	paused := map[string]CredPauseView{
		"ns/a": {Credential: "ns/a"},
		"ns/b": {Credential: "ns/b"},
	}
	// First unheld candidate wins, scanned in order.
	if got, ok := SelectAlternate([]string{"ns/a", "ns/c", "ns/d"}, paused); !ok || got != "ns/c" {
		t.Fatalf("SelectAlternate = (%q,%v), want (ns/c,true)", got, ok)
	}
	// All held ⇒ no alternate.
	if _, ok := SelectAlternate([]string{"ns/a", "ns/b"}, paused); ok {
		t.Fatalf("all-held must return ok=false")
	}
	// Empty paused set ⇒ first candidate.
	if got, ok := SelectAlternate([]string{"ns/z"}, nil); !ok || got != "ns/z" {
		t.Fatalf("empty paused: got (%q,%v), want (ns/z,true)", got, ok)
	}
	// No candidates ⇒ no alternate.
	if _, ok := SelectAlternate(nil, paused); ok {
		t.Fatalf("no candidates must return ok=false")
	}
}

func TestEarliestResume(t *testing.T) {
	t0 := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(30 * time.Second)
	t2 := t0.Add(90 * time.Second)

	// Minimum timer horizon across timer-mode holds.
	paused := map[string]CredPauseView{
		"ns/a": {Credential: "ns/a", ResumeAt: &t2},
		"ns/b": {Credential: "ns/b", ResumeAt: &t1},
		"ns/c": {Credential: "ns/c", ResumeAt: nil}, // refresh-mode: invisible to the clock
	}
	got := EarliestResume(paused)
	if got == nil || !got.Equal(t1) {
		t.Fatalf("EarliestResume = %v, want %v", got, t1)
	}

	// All refresh-mode ⇒ nil (no clock to wait on).
	refresh := map[string]CredPauseView{"ns/x": {ResumeAt: nil}, "ns/y": {ResumeAt: nil}}
	if got := EarliestResume(refresh); got != nil {
		t.Fatalf("all refresh-mode must give nil horizon, got %v", got)
	}
	if got := EarliestResume(nil); got != nil {
		t.Fatalf("empty set must give nil horizon, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Shim event ↔ step ↔ reason mappings (7.4)
// ---------------------------------------------------------------------------

func TestSignalStepAndEventReasonRoundTrip(t *testing.T) {
	cases := []struct {
		ev     CredentialEvent
		step   string
		reason CredPauseReason
	}{
		{EventRateLimited, "paused(rate_limited)", ReasonRateLimited},
		{EventExpired, "paused(credential_expired)", ReasonCredentialExpired},
		{EventRotated, "paused(credential_rotated)", ReasonCredentialRotated},
		{EventUnreachable, "paused(endpoint_unreachable)", ReasonEndpointUnreachable},
	}
	for _, tc := range cases {
		if got := string(SignalStep(tc.ev)); got != tc.step {
			t.Errorf("SignalStep(%q) = %q, want %q", tc.ev, got, tc.step)
		}
		if got := EventReason(tc.ev); got != tc.reason {
			t.Errorf("EventReason(%q) = %q, want %q", tc.ev, got, tc.reason)
		}
		if !tc.reason.Valid() {
			t.Errorf("mapped reason %q must be valid", tc.reason)
		}
	}
	// Fail-closed on an unknown event.
	if SignalStep("boom") != "" {
		t.Errorf("unknown event must map to empty step")
	}
	if EventReason("boom") != "" {
		t.Errorf("unknown event must map to empty reason")
	}
}

// ---------------------------------------------------------------------------
// CredentialSignal validation (fail-closed, before any DB work)
// ---------------------------------------------------------------------------

func rlDur(d time.Duration) *time.Duration { return &d }

func TestCredentialSignalValidate(t *testing.T) {
	ok := CredentialSignal{
		Run:        "run-1",
		Item:       "item-1",
		Credential: CredentialRef{ID: "ns/alice", Principal: "alice", Class: ClassClaudeOAuth},
		Event:      EventExpired,
	}
	if err := ok.validate(); err != nil {
		t.Fatalf("valid signal rejected: %v", err)
	}
	rl := ok
	rl.Event = EventRateLimited
	rl.RetryAfter = rlDur(30 * time.Second)
	if err := rl.validate(); err != nil {
		t.Fatalf("valid rate_limited signal rejected: %v", err)
	}

	bad := []struct {
		name string
		mut  func(s *CredentialSignal)
	}{
		{"no run", func(s *CredentialSignal) { s.Run = "" }},
		{"no credential id", func(s *CredentialSignal) { s.Credential.ID = "" }},
		{"bad class", func(s *CredentialSignal) { s.Credential.Class = "kerberos" }},
		{"unknown event", func(s *CredentialSignal) { s.Event = "boom" }},
		{"retry-after on non-rate-limit", func(s *CredentialSignal) { s.Event = EventExpired; s.RetryAfter = rlDur(time.Second) }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			s := ok
			tc.mut(&s)
			if err := s.validate(); err == nil {
				t.Fatalf("signal %q must be rejected", tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PauseCredential fail-closed branches (return before BeginTx, so no live DB
// connection is required — the handle is never used).
// ---------------------------------------------------------------------------

func offlineStore(t *testing.T) *CredentialPauseStore {
	t.Helper()
	db := sql.OpenDB(offlineConnector{})
	t.Cleanup(func() { _ = db.Close() })
	cfg := DefaultCredPauseConfig()
	cfg.Pause = "coord.credential_pause"
	s, err := NewCredentialPauseStore(db, cfg, func() float64 { return 0.5 })
	if err != nil {
		t.Fatalf("NewCredentialPauseStore: %v", err)
	}
	return s
}

func TestNewCredentialPauseStoreGuards(t *testing.T) {
	cfg := DefaultCredPauseConfig()
	cfg.Pause = "coord.credential_pause"
	if _, err := NewCredentialPauseStore(nil, cfg, nil); err == nil {
		t.Fatalf("nil db must be rejected")
	}
	if _, err := NewCredentialPauseStore(offlineStore(t).db, CredPauseConfig{}, nil); err == nil {
		t.Fatalf("invalid config must be rejected")
	}
}

func TestPauseCredentialValidationBranches(t *testing.T) {
	s := offlineStore(t)
	ctx := context.Background()
	cred := CredentialRef{ID: "ns/alice", Principal: "alice", Class: ClassClaudeOAuth}

	cases := []struct {
		name string
		req  CredPauseRequest
	}{
		{"invalid reason", CredPauseRequest{Credential: cred, Reason: "boom"}},
		{"invalid class", CredPauseRequest{Credential: CredentialRef{ID: "ns/a", Class: "kerberos"}, Reason: ReasonCredentialExpired}},
		{"empty credential id", CredPauseRequest{Credential: CredentialRef{Class: ClassAPIKey}, Reason: ReasonCredentialExpired}},
		{"retry-after on refresh reason", CredPauseRequest{Credential: cred, Reason: ReasonCredentialExpired, RetryAfter: rlDur(time.Second)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.PauseCredential(ctx, tc.req); err == nil {
				t.Fatalf("PauseCredential(%q) must be rejected", tc.name)
			}
		})
	}
}

func TestResumeOnRefreshEmptyID(t *testing.T) {
	s := offlineStore(t)
	if _, err := s.ResumeOnRefresh(context.Background(), ""); err == nil {
		t.Fatalf("empty credential id must be rejected")
	}
}

// ---------------------------------------------------------------------------
// ApplyCredentialSignal fail-closed branches (return before BeginTx)
// ---------------------------------------------------------------------------

func TestApplyCredentialSignalValidationBranches(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultCredPauseConfig()
	cfg.Pause = "coord.credential_pause"
	db := offlineStore(t).db
	sig := CredentialSignal{Run: "run-1", Item: "item-1",
		Credential: CredentialRef{ID: "ns/alice", Principal: "alice", Class: ClassClaudeOAuth},
		Event:      EventExpired}

	if _, err := ApplyCredentialSignal(ctx, nil, cfg, "shim", "proj", sig); err == nil {
		t.Fatalf("nil db must be rejected")
	}
	if _, err := ApplyCredentialSignal(ctx, db, CredPauseConfig{}, "shim", "proj", sig); err == nil {
		t.Fatalf("invalid config must be rejected")
	}
	badSig := sig
	badSig.Run = ""
	if _, err := ApplyCredentialSignal(ctx, db, cfg, "shim", "proj", badSig); err == nil {
		t.Fatalf("invalid signal must be rejected")
	}
	if _, err := ApplyCredentialSignal(ctx, db, cfg, "", "proj", sig); err == nil {
		t.Fatalf("empty principal must be rejected")
	}
	if _, err := ApplyCredentialSignal(ctx, db, cfg, "shim", "", sig); err == nil {
		t.Fatalf("empty projectID must be rejected")
	}
}

// ---------------------------------------------------------------------------
// stepList renders the live set as a string array for the pg ANY() bind.
// ---------------------------------------------------------------------------

func TestStepListRendersStrings(t *testing.T) {
	out, ok := stepList(pausedFromSteps).([]string)
	if !ok {
		t.Fatalf("stepList must render a []string, got %T", stepList(pausedFromSteps))
	}
	if len(out) != len(pausedFromSteps) {
		t.Fatalf("stepList length = %d, want %d", len(out), len(pausedFromSteps))
	}
	joined := strings.Join(out, ",")
	for _, want := range []string{"running", "paused(rate_limited)", "paused(credential_expired)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("stepList missing %q", want)
		}
	}
}

// guard: the exported error strings name the credential, not an opaque failure.
func TestPauseErrorsNameAttribution(t *testing.T) {
	s := offlineStore(t)
	_, err := s.PauseCredential(context.Background(),
		CredPauseRequest{Credential: CredentialRef{Class: ClassAPIKey}, Reason: ReasonCredentialExpired})
	if err == nil || !strings.Contains(err.Error(), "attribution") {
		t.Fatalf("empty-id error must explain attribution is required, got %v", err)
	}
}
