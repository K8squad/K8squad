// credpause.go — the per-credential pause/resume ledger (Stories 7.4+7.6 /
// ISI-2898, arch §10 pause/resume, §11 per-user credentials, §7.2
// credentialLifecycle). It reuses the Story 2.11/3.7 pause machinery's
// discipline — SINGLE DURABLE WAKE, crash-safe re-derivation, equal-jitter
// backoff, SKIP LOCKED exactly-once claims (pkg/coord/resume.go) — and extends
// it along the two axes the credential stories add:
//
//   - ATTRIBUTION (7.6): episodes are keyed by CREDENTIAL, not work item. A
//     Paused(rate_limited) triggered by the shim's credentialLifecycle signal
//     (5.10) is attributed to the specific per-user Secret/principal whose
//     subscription was throttled, with Retry-After tracked PER CREDENTIAL —
//     two credentials on the same item keep independent windows, and the
//     coordinator's re-route advisory (2.10) can pick an Agent bound to a
//     DIFFERENT, unpaused credential.
//
//   - REASON FAMILY + RESUME MODE (7.4): one pause machinery, four legible
//     reasons — rate_limited (timer resume at now+Retry-After, the §8 tier-2
//     wake), credential_expired, credential_rotated and endpoint_unreachable
//     (NO timer: they clear via ResumeOnRefresh, the write the 7.7 credential
//     controller / a Secret rotation makes when fresh material lands). An
//     unreachable BYO endpoint (7.5) is therefore a legible Paused, never an
//     opaque dial failure — and every model the credential stories pin
//     (Claude-OAuth 7.2, static API key 7.3, BYO endpoint 7.5) holds the SAME
//     contract, because the ledger keys on the per-user Secret identity, not
//     on any provider's error shape.
//
// # Schema binding
//
// Same discipline as resume.go: statements are parameterised only by a
// CredPauseConfig binding (table names + policy), values travel as bound
// parameters, and the production schema is db/migrations/0010_credential_pause.sql
// (coord.credential_pause + the reconcile_step CHECK extension for the new
// paused(reason) steps). NewCredPauseForTest provisions the self-contained
// harness schema the chaos gate drives.
package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// Reason taxonomy (7.4/7.6: one family, distinct legible reasons)
// ---------------------------------------------------------------------------

// CredPauseReason is the closed reason family for credential-driven pauses.
// Every member is a legible operator signal (FR-F6) in the same pause/resume
// machinery; they differ only in resume mode (timer vs refresh).
type CredPauseReason string

const (
	// ReasonRateLimited (7.6): the credential's subscription was throttled.
	// Timer-resumable at now + Retry-After (tracked per credential).
	ReasonRateLimited CredPauseReason = "rate_limited"
	// ReasonCredentialExpired (7.4): OAuth refresh window lapsed or a static
	// key was revoked. Refresh-resumed (7.7 controller write-back).
	ReasonCredentialExpired CredPauseReason = "credential_expired"
	// ReasonCredentialRotated (7.4): the Secret rotated mid-Run. Refresh-resumed
	// once the new material propagates.
	ReasonCredentialRotated CredPauseReason = "credential_rotated"
	// ReasonEndpointUnreachable (7.4/7.5): the BYO/Ollama endpoint credential's
	// endpoint is unreachable. Refresh-resumed (Secret update / endpoint back).
	ReasonEndpointUnreachable CredPauseReason = "endpoint_unreachable"
)

// TimerResumed reports whether the reason resumes on the single durable wake
// (rate_limited: resume_at = now + Retry-After) as opposed to clearing on a
// credential refresh/rotation write (ResumeOnRefresh). This is the 7.4 split:
// the platform never fails opaquely, and it never fabricates a timer for a hold
// only fresh credential material can clear.
func (r CredPauseReason) TimerResumed() bool { return r == ReasonRateLimited }

// Valid rejects an out-of-family reason up front (fail-closed).
func (r CredPauseReason) Valid() bool {
	switch r {
	case ReasonRateLimited, ReasonCredentialExpired, ReasonCredentialRotated, ReasonEndpointUnreachable:
		return true
	}
	return false
}

// CredentialClass is the credential-model tag (arch §10/§11): every story-pinned
// model — Claude-family OAuth (7.2), second-runtime static API key (7.3) and the
// BYO/Ollama endpoint shape (7.5) — rides the SAME pause machinery; the class is
// recorded for attribution/observability, never for special-cased behaviour.
type CredentialClass string

const (
	ClassClaudeOAuth CredentialClass = "claude_oauth"  // 7.2: per-user OAuth seat token
	ClassAPIKey      CredentialClass = "api_key"       // 7.3: long-lived provider key
	ClassBYOEndpoint CredentialClass = "byo_endpoint"  // 7.5: endpoint URL (+ optional token)
)

// Valid rejects an out-of-model class up front (fail-closed).
func (c CredentialClass) Valid() bool {
	switch c {
	case ClassClaudeOAuth, ClassAPIKey, ClassBYOEndpoint:
		return true
	}
	return false
}

// CredentialRef identifies ONE per-user credential (arch §11: a per-user k8s
// Secret ref — never a shared master). ID is "namespace/secret-name" (the
// Secret identity), Principal the owning seat/service principal. This is the
// attribution key of the whole ledger: a pause names exactly this credential.
type CredentialRef struct {
	ID     string          // "namespace/name" of the per-user Secret
	Principal string       // owning human seat / service principal
	Class  CredentialClass // claude_oauth | api_key | byo_endpoint
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

// CredPauseConfig binds the credential-pause statements to a concrete schema.
// Pause is the (schema-qualified) credential-pause ledger table; the backoff
// knobs carry the same semantics as ResumeConfig (they bind only when a
// rate_limited signal arrives WITHOUT a Retry-After).
type CredPauseConfig struct {
	Pause        string
	BackoffBase  time.Duration
	BackoffCap   time.Duration
	BackoffReset time.Duration
	ResumeBatch  int
}

// DefaultCredPauseConfig pins the v1 policy to the same numbers as the §8
// tier-2 resume engine (DefaultResumeConfig): one policy, two ledgers.
func DefaultCredPauseConfig() CredPauseConfig {
	return CredPauseConfig{
		BackoffBase:  1 * time.Second,
		BackoffCap:   5 * time.Minute,
		BackoffReset: 10 * time.Minute,
		ResumeBatch:  64,
	}
}

func (c CredPauseConfig) validate() error {
	if c.Pause == "" {
		return errors.New("coord.CredPauseConfig: Pause table is required")
	}
	if c.BackoffBase <= 0 || c.BackoffCap <= 0 || c.BackoffReset <= 0 {
		return fmt.Errorf("coord.CredPauseConfig: BackoffBase, BackoffCap and "+
			"BackoffReset must all be > 0 (got base=%s cap=%s reset=%s)",
			c.BackoffBase, c.BackoffCap, c.BackoffReset)
	}
	if c.BackoffCap < c.BackoffBase {
		return fmt.Errorf("coord.CredPauseConfig: BackoffCap (%s) < BackoffBase (%s)",
			c.BackoffCap, c.BackoffBase)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// CredPauseRequest is one credential-lifecycle observation to record. Item/Run
// carry WHERE the signal surfaced (the operator signal's provenance); the
// episode itself is keyed by the credential — attribution, not incidence.
type CredPauseRequest struct {
	Credential CredentialRef
	Reason     CredPauseReason
	RetryAfter *time.Duration // provider window; rate_limited only, nil ⇒ backoff
	Item       string         // work_item_id provenance ("" ⇒ NULL)
	Run        string         // run_id provenance      ("" ⇒ NULL)
}

// CredPauseInfo is the durable outcome of a PauseCredential call.
type CredPauseInfo struct {
	Reason     CredPauseReason
	Attempt    int
	ResumeAt   *time.Time     // nil ⇔ refresh-resumed (no timer)
	RetryAfter *time.Duration // as-recorded provider window, nil when backoff used
}

// CredDuePause is one claimed (exactly-once) timer resume of a credential.
type CredDuePause struct {
	Credential string
	Principal  string
	Attempt    int
	ResumeAt   time.Time
	RetryAfter *time.Duration
}

// CredPauseView is the advisory read for the coordinator re-route (2.10) and
// the console (8.6/8.11): which credential is held, why, and until when.
type CredPauseView struct {
	Credential string
	Principal  string
	Class      CredentialClass
	Reason     CredPauseReason
	ResumeAt   *time.Time // nil ⇔ refresh-driven hold
	PausedAt   time.Time
}

// CredentialPauseStore runs the per-credential ledger against the bound schema.
// It implements the same wakeSource contract as ResumeStore, so the Story 3.7
// Timer (NewTimer) drives it unchanged — one wake loop, two ledgers, the same
// no-polling/crash-safe/exactly-once properties.
type CredentialPauseStore struct {
	db   *sql.DB
	cfg  CredPauseConfig
	rand func() float64
}

// NewCredentialPauseStore binds the ledger. rand may be nil (defaults to the
// package source); a deterministic source makes the jitter envelope exactly
// assertable in tests.
func NewCredentialPauseStore(db *sql.DB, cfg CredPauseConfig, rand func() float64) (*CredentialPauseStore, error) {
	if db == nil {
		return nil, errors.New("coord.NewCredentialPauseStore: nil db")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if rand == nil {
		rand = func() float64 { return randv2() }
	}
	return &CredentialPauseStore{db: db, cfg: cfg, rand: rand}, nil
}

// PauseCredential durably records (or refreshes) the credential's pause
// episode. rate_limited plans resume_at on the DB clock exactly like
// ResumeStore.Pause (Retry-After honoured verbatim; equal-jitter backoff
// otherwise, escalating across consecutive streaks, refresh-in-place keeps the
// attempt — the provider re-sent a window for the SAME hold). The refresh-mode
// reasons record resume_at NULL: no timer is fabricated for a hold only fresh
// credential material can clear.
//
// The attribution is idempotent per credential: a re-signalled pending episode
// rewrites the row in place (new Retry-After/provenance, same attempt streak);
// a signal after a resume escalates or resets the streak per BackoffReset.
func (s *CredentialPauseStore) PauseCredential(ctx context.Context, req CredPauseRequest) (CredPauseInfo, error) {
	if !req.Reason.Valid() {
		return CredPauseInfo{}, fmt.Errorf("coord.PauseCredential: invalid reason %q (family: rate_limited|credential_expired|credential_rotated|endpoint_unreachable)", req.Reason)
	}
	if !req.Credential.Class.Valid() {
		return CredPauseInfo{}, fmt.Errorf("coord.PauseCredential: invalid credential class %q (models: claude_oauth|api_key|byo_endpoint)", req.Credential.Class)
	}
	if req.Credential.ID == "" {
		return CredPauseInfo{}, errors.New("coord.PauseCredential: credential ID (per-user Secret \"namespace/name\") is required — attribution is the point (7.6)")
	}
	if req.Reason != ReasonRateLimited && req.RetryAfter != nil {
		return CredPauseInfo{}, fmt.Errorf("coord.PauseCredential: Retry-After applies to rate_limited only (got reason=%s) — expiry/rotation/unreachable resume on refresh, not a timer", req.Reason)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CredPauseInfo{}, fmt.Errorf("coord.PauseCredential: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var priorAttempt sql.NullInt32
	var priorResumedAt sql.NullTime
	read := fmt.Sprintf(
		`SELECT attempt, resumed_at FROM %s WHERE credential_id=$1 FOR UPDATE`, s.cfg.Pause)
	switch err := tx.QueryRowContext(ctx, read, req.Credential.ID).Scan(&priorAttempt, &priorResumedAt); err {
	case nil, sql.ErrNoRows:
		// new episode below
	default:
		return CredPauseInfo{}, fmt.Errorf("coord.PauseCredential: read prior: %w", err)
	}

	now := dbNow(ctx, tx)
	attempt := planAttempt(priorAttempt, priorResumedAt, now, s.cfg.BackoffReset)

	var resumeAt *time.Time
	var retryAfter *time.Duration
	if req.Reason.TimerResumed() {
		if req.RetryAfter != nil && *req.RetryAfter > 0 {
			retryAfter = req.RetryAfter
		} else {
			d := EqualJitter(s.cfg.BackoffBase, s.cfg.BackoffCap, attempt, s.rand())
			retryAfter = &d
		}
		at := now.Add(*retryAfter)
		resumeAt = &at
	}

	var item, run any
	if req.Item != "" {
		item = req.Item
	}
	if req.Run != "" {
		run = req.Run
	}
	var raMs any
	if req.Reason.TimerResumed() {
		raMs = retryAfter.Milliseconds()
	}

	write := fmt.Sprintf(
		`INSERT INTO %s
		     (credential_id, principal, credential_class, reason,
		      work_item_id, run_id, retry_after_ms, attempt, resume_at, paused_at, resumed_at, refreshed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULL, NULL)
		 ON CONFLICT (credential_id) DO UPDATE SET
		     principal       = EXCLUDED.principal,
		     credential_class= EXCLUDED.credential_class,
		     reason          = EXCLUDED.reason,
		     work_item_id    = EXCLUDED.work_item_id,
		     run_id          = EXCLUDED.run_id,
		     retry_after_ms  = EXCLUDED.retry_after_ms,
		     attempt         = EXCLUDED.attempt,
		     resume_at       = EXCLUDED.resume_at,
		     paused_at       = EXCLUDED.paused_at,
		     resumed_at      = NULL,
		     refreshed_at    = NULL`,
		s.cfg.Pause)
	if _, err := tx.ExecContext(ctx, write,
		req.Credential.ID, req.Credential.Principal, string(req.Credential.Class), string(req.Reason),
		item, run, raMs, attempt, resumeAt, now); err != nil {
		return CredPauseInfo{}, fmt.Errorf("coord.PauseCredential: write: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return CredPauseInfo{}, fmt.Errorf("coord.PauseCredential: commit: %w", err)
	}
	return CredPauseInfo{Reason: req.Reason, Attempt: attempt, ResumeAt: resumeAt, RetryAfter: retryAfter}, nil
}

// ResumeOnRefresh is the 7.4 resume path for refresh-mode holds: the 7.7
// credential controller (or a Secret rotation) wrote fresh material back, so
// the pending episode clears NOW — idempotent, and never fabricates a resume
// for an already-resumed episode (wasPending=false ⇔ nothing was held).
// Refreshing a credential with NO pending episode is a no-op success, so a
// rotation write-back racing a timer resume cannot resurrect the hold.
func (s *CredentialPauseStore) ResumeOnRefresh(ctx context.Context, credentialID string) (wasPending bool, err error) {
	if credentialID == "" {
		return false, errors.New("coord.ResumeOnRefresh: credential ID is required")
	}
	q := fmt.Sprintf(
		`UPDATE %s SET resumed_at = clock_timestamp(), refreshed_at = clock_timestamp()
		  WHERE credential_id = $1 AND resumed_at IS NULL`, s.cfg.Pause)
	n, err := s.db.ExecContext(ctx, q, credentialID)
	if err != nil {
		return false, fmt.Errorf("coord.ResumeOnRefresh: %w", err)
	}
	affected, err := n.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("coord.ResumeOnRefresh: rows: %w", err)
	}
	return affected > 0, nil
}

// NextWake returns the EARLIEST pending timer-resumable deadline. Refresh-mode
// episodes (resume_at NULL) are invisible here — that is 7.4's guarantee that
// no timer fires for a credential waiting on fresh material.
func (s *CredentialPauseStore) NextWake(ctx context.Context) (time.Time, bool, error) {
	q := fmt.Sprintf(
		`SELECT resume_at FROM %s
		  WHERE resumed_at IS NULL AND resume_at IS NOT NULL
		  ORDER BY resume_at LIMIT 1`, s.cfg.Pause)
	var at time.Time
	switch err := s.db.QueryRowContext(ctx, q).Scan(&at); err {
	case nil:
		return at, true, nil
	case sql.ErrNoRows:
		return time.Time{}, false, nil
	default:
		return time.Time{}, false, fmt.Errorf("coord.CredentialPauseStore.NextWake: %w", err)
	}
}

// ResumeDue claims every timer-resumable episode whose deadline has been
// reached — the SAME single-statement SKIP LOCKED exactly-once discipline as
// ResumeStore.ResumeDue, partitioning concurrent timers so each credential
// resume fires at most once process-crash or replica-race notwithstanding.
func (s *CredentialPauseStore) ResumeDue(ctx context.Context) ([]CredDuePause, error) {
	q := fmt.Sprintf(
		`WITH due AS (
		     SELECT credential_id FROM %s
		      WHERE resumed_at IS NULL AND resume_at IS NOT NULL
		        AND resume_at <= clock_timestamp()
		      ORDER BY resume_at
		      FOR UPDATE SKIP LOCKED
		      LIMIT $1)
		 UPDATE %s p SET resumed_at = clock_timestamp()
		  FROM due WHERE p.credential_id = due.credential_id
		  RETURNING p.credential_id, p.principal, p.attempt, p.resume_at, p.retry_after_ms`,
		s.cfg.Pause, s.cfg.Pause)
	rows, err := s.db.QueryContext(ctx, q, s.cfg.batchParam())
	if err != nil {
		return nil, fmt.Errorf("coord.CredentialPauseStore.ResumeDue: %w", err)
	}
	defer rows.Close()

	var out []CredDuePause
	for rows.Next() {
		var d CredDuePause
		var raMs sql.NullInt64
		if err := rows.Scan(&d.Credential, &d.Principal, &d.Attempt, &d.ResumeAt, &raMs); err != nil {
			return nil, fmt.Errorf("coord.CredentialPauseStore.ResumeDue: scan: %w", err)
		}
		if raMs.Valid {
			ra := time.Duration(raMs.Int64) * time.Millisecond
			d.RetryAfter = &ra
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PausedSet is the advisory read (2.10 re-route, 8.6/8.11 console): every
// PENDING episode with its reason and horizon. A credential absent from this
// set is not held.
func (s *CredentialPauseStore) PausedSet(ctx context.Context) (map[string]CredPauseView, error) {
	q := fmt.Sprintf(
		`SELECT credential_id, principal, credential_class, reason, resume_at, paused_at
		   FROM %s WHERE resumed_at IS NULL`, s.cfg.Pause)
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("coord.PausedSet: %w", err)
	}
	defer rows.Close()

	out := map[string]CredPauseView{}
	for rows.Next() {
		var v CredPauseView
		var resumeAt sql.NullTime
		if err := rows.Scan(&v.Credential, &v.Principal, &v.Class, &v.Reason, &resumeAt, &v.PausedAt); err != nil {
			return nil, fmt.Errorf("coord.PausedSet: scan: %w", err)
		}
		if resumeAt.Valid {
			at := resumeAt.Time
			v.ResumeAt = &at
		}
		out[v.Credential] = v
	}
	return out, rows.Err()
}

// DB exposes the backing handle (same surface as ResumeStore).
func (s *CredentialPauseStore) DB() *sql.DB { return s.db }

func (c CredPauseConfig) batchParam() int {
	if c.ResumeBatch <= 0 {
		return math.MaxInt32 // unlimited (PG LIMIT NULL is not universally usable)
	}
	return c.ResumeBatch
}

// SelectAlternate is the pure re-route advisory (7.6 → Story 2.10): given the
// candidate (agent → credential) bindings and the current paused set, it picks
// a credential that is NOT held, so the coordinator routes around the throttle.
// Deterministic: candidates are scanned in order and the first unheld
// credential wins; all-held returns false (the caller waits on the earliest
// resume horizon, which PausedSet supplies).
func SelectAlternate(candidates []string, paused map[string]CredPauseView) (string, bool) {
	for _, c := range candidates {
		if _, held := paused[c]; !held {
			return c, true
		}
	}
	return "", false
}

// EarliestResume is the pure scheduling hint for the all-held case: the minimum
// timer horizon across the paused views (nil when every hold is refresh-driven
// — the caller is waiting on credential material, not a clock).
func EarliestResume(paused map[string]CredPauseView) *time.Time {
	var best *time.Time
	ids := make([]string, 0, len(paused))
	for id := range paused {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic scan order
	for _, id := range ids {
		v := paused[id]
		if v.ResumeAt == nil {
			continue
		}
		if best == nil || v.ResumeAt.Before(*best) {
			at := *v.ResumeAt
			best = &at
		}
	}
	return best
}

// NewCredPauseForTest binds the ledger to the self-contained harness schema
// the chaos gate provisions, mirroring NewResumeForTest.
func NewCredPauseForTest(db *sql.DB, rand func() float64) (*CredentialPauseStore, error) {
	ctx := context.Background()
	for _, s := range []string{
		`DROP TABLE IF EXISTS credential_pause`,
		`CREATE TABLE credential_pause (
		     credential_id    text        PRIMARY KEY,
		     principal        text        NOT NULL DEFAULT '',
		     credential_class text        NOT NULL,
		     reason           text        NOT NULL,
		     work_item_id     text,
		     run_id           text,
		     retry_after_ms   bigint,
		     attempt          int         NOT NULL,
		     resume_at        timestamptz,
		     paused_at        timestamptz NOT NULL,
		     resumed_at       timestamptz,
		     refreshed_at     timestamptz)`,
		`CREATE INDEX idx_credential_pause_pending ON credential_pause (resume_at)
		     WHERE resumed_at IS NULL AND resume_at IS NOT NULL`,
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return nil, fmt.Errorf("coord.NewCredPauseForTest: provision: %w", err)
		}
	}
	cfg := DefaultCredPauseConfig()
	cfg.Pause = "credential_pause"
	cfg.BackoffBase = 50 * time.Millisecond
	cfg.BackoffCap = 800 * time.Millisecond
	cfg.BackoffReset = 500 * time.Millisecond
	cfg.ResumeBatch = 64
	return NewCredentialPauseStore(db, cfg, rand)
}
