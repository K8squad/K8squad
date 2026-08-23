// prodreroute.go — the Story 2.10 rate-limit RE-ROUTE BOUND TO THE PRODUCTION
// SCHEMA (ISI-2882, gap ISI-2876): when a Run Paused(rate_limited) ESCALATES
// (repeat/long window, 3.7), the squad moves the item to another Agent with a
// DIFFERENT credential so one provider rate limit cannot stall the squad.
//
// The story's [no-P2P guardrail] pins the protocol: a re-route is
//
//	FENCED RELEASE  →  COORDINATOR RE-DISPATCH  →  §6.2 CLAIM
//
// — never a peer-to-peer handoff of the lease. Same custody discipline as
// reclaim (2.4, prodreconcile.go Reclaim) and handoff (2.8, prodhandoff.go):
// custody only ever moves through the record, and this file adds NO new custody
// surface. Concretely, one transaction in ReleaseForReroute:
//
//	(0) idempotency pre-check — a hold row already naming this run is a
//	    convergent no-op (AlreadyReleased; the §6.4 discipline);
//	(1) custody guard — coord.claim must still name (holder, run) as the live
//	    holder of the paused Run's checkout; nobody releases a checkout they do
//	    not hold (FOR UPDATE serialises against a concurrent §6.3 reclaim);
//	(2) FENCED RELEASE — fence_token bumped, holder/run/lease cleared. The
//	    original holder is fenced out: its zombie writes fail the fence-equality
//	    / holder guards (it cannot resume-clobber — proven by the chaos gate via
//	    the 2.8 custody-gated handoff write);
//	(3) RE-DISPATCH — work_item.state in_progress → todo, so the §6.2 pop can
//	    fan the item out again (the 2.9 coordinator's re-dispatch decision is
//	    recorded as provenance on the §6.5 audit row);
//	(4) HOLD — coord.rate_limit_reroute (0009) records WHICH credential was
//	    throttled and until when (the Retry-After window, 3.7);
//	(5) §6.5 AUDIT — one claim_released row with the re-route payload, and —
//	    when enabled — the 12.1 outbox event, co-committed (same discipline as
//	    the 2.2 claim).
//
// # The different-credential claim (7.6): the claiming side of the guard
//
// The hold is load-bearing on the CLAIM side. ProdClaimer's credentialed
// variants (ClaimNextCredentialed / AcquireSpecificCredentialed, also in this
// file) refuse an item while a LIVE hold (resume_at still future) pins the
// credential the claimant presents. So the re-routed item can only be claimed
// by an Agent presenting a DIFFERENT credential key — the "other Agent claims
// it" half of the story — and once the Retry-After window clears the hold is
// inert by predicate: the SAME credential may claim again, which is exactly the
// 3.7 auto-resume branch ("if no alternate credential exists, the item stays
// paused until the window clears").
//
// The escalation DECISION and the alternate-credential LOOKUP are pure and
// caller-side: ShouldReroute evaluates the 3.7 policy (repeat attempts or a
// long Retry-After window) on the durable pause episode, and
// PickAlternateCredential answers "does the squad have another Agent whose
// credential differs" over the caller's squad roster (the k8s-side Agent →
// credential resolution: each Agent's spec.credentialSecretRef, projected to an
// opaque key by the informer cache). ReleaseForReroute is only to be called
// when an alternate exists — when PickAlternateCredential says no, the item
// stays paused and the 2.11 resume timer (resume.go) owns the wake.
//
// # Seat-model (§11.2 / 7.8 / ADR-041): a credential is never re-pointed
//
// throttled_credential and the claimant's credential key are OPAQUE IDENTITIES
// (e.g. "namespace/secret-name"), never token material. The re-route moves the
// WORK to a different Agent; each Agent always authenticates with ITS OWN
// bound credential (per-credential attribution by construction, 7.6). There is
// no code path here that could re-point one human-seat token to serve a
// different principal: nothing stores or pairs principals with foreign
// credentials — the hold only names the identity that was throttled so a
// same-credential claim can be refused.
//
// # Boundary notes (consumed, not redefined)
//
//   - The pause episodes and their escalation attempts are Story 2.11/3.7's
//     (resume.go); this file consumes the (attempt, retryAfter) facts.
//   - The coordinator's squad-lead AUTHORIZATION is admission's concern (Role
//     CRD / apiserver RBAC, same as 2.9): the spine records the coordinator
//     principal for §6.5 provenance.
//   - The k8s Run-reconciler wiring (moving the CRD to Paused(rate_limited)
//     and calling into this seam) is the Epic 3/7 reconcile path; this file is
//     the coordination-layer mechanism it drives.
package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/K8squad/K8squad/pkg/events"
)

// ---------------------------------------------------------------------------
// Policy: when does an escalated pause re-route? (pure, 3.7)
// ---------------------------------------------------------------------------

// ReroutePolicy pins the 3.7 escalation predicate that releases the checkout
// for re-routing: a pause ESCALATES when it repeats (attempt >= AfterAttempts)
// or its provider Retry-After window is long (>= MinWindow). Short first-time
// pauses RETAIN the checkout (lease heartbeat continues) — only escalation
// releases it for re-routing.
type ReroutePolicy struct {
	// AfterAttempts is the repeat threshold: an episode at (or past) this
	// escalation attempt re-routes. 2 = the second consecutive episode.
	AfterAttempts int

	// MinWindow is the long-window threshold: a provider Retry-After at least
	// this long re-routes immediately (waiting it out would stall the squad
	// longer than re-dispatching around the credential).
	MinWindow time.Duration
}

// DefaultReroutePolicy is the v1 policy: re-route on the second escalation
// attempt, or on any Retry-After window of ten minutes or more. It is the ONLY
// sanctioned source of a ReroutePolicy — callers must not hand-build the struct
// (a zero-value ReroutePolicy{} fails open, see Validate).
func DefaultReroutePolicy() ReroutePolicy {
	return ReroutePolicy{AfterAttempts: 2, MinWindow: 10 * time.Minute}
}

// Validate rejects a policy that would fail open (F1/ISI-3083, Cursor F4). A
// zero-value or AfterAttempts<2 policy re-routes EVERY first, short pause:
// ShouldReroute clamps attempt to >= 1 and then tests attempt >= AfterAttempts,
// so any threshold below 2 fires on the first episode — the exact opposite of
// the 3.7 "short first-time pauses RETAIN the checkout" contract. MinWindow must
// be positive so the long-window arm has a real threshold (a zero window would
// re-route on any Retry-After). Call this on any policy not obtained from
// DefaultReroutePolicy before driving a release with it.
func (p ReroutePolicy) Validate() error {
	if p.AfterAttempts < 2 {
		return fmt.Errorf("coord.ReroutePolicy: AfterAttempts must be >= 2 (got %d) — "+
			"a lower threshold re-routes every first, short pause (3.7 retains those)", p.AfterAttempts)
	}
	if p.MinWindow <= 0 {
		return fmt.Errorf("coord.ReroutePolicy: MinWindow must be positive (got %s)", p.MinWindow)
	}
	return nil
}

// ShouldReroute is the pure 3.7 escalation verdict for one pause episode:
// repeat (attempt >= policy.AfterAttempts) OR long window (retryAfter present
// and >= policy.MinWindow). attempt is clamped >= 1; a nil/zero Retry-After
// never triggers the long-window arm (the 2.11 backoff clock owns that case).
// The verdict is only meaningful for a Validate-clean policy; an unvalidated
// zero-value policy fails open (that is what Validate exists to reject).
func ShouldReroute(attempt int, retryAfter *time.Duration, policy ReroutePolicy) bool {
	if attempt < 1 {
		attempt = 1
	}
	if attempt >= policy.AfterAttempts {
		return true
	}
	return retryAfter != nil && *retryAfter >= policy.MinWindow
}

// PickAlternateCredential answers the squad-roster question of 7.6/2.10: given
// the throttled credential and the candidate credentials of the squad's other
// claimable Agents (caller-resolved, in the coordinator's priority order),
// which — if any — differs? The first differing candidate wins (deterministic,
// coordinator-ordered). ("", false): no alternate exists — the item stays
// paused until the Retry-After window clears (3.7 auto-resume).
func PickAlternateCredential(throttled string, candidates []string) (string, bool) {
	for _, c := range candidates {
		if c != "" && c != throttled {
			return c, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Release: fenced release → re-dispatch → hold, one transaction
// ---------------------------------------------------------------------------

// Sentinel errors for the release guards. Callers match with errors.Is.
var (
	// ErrNotRerouteCustodian: the claim row does not name (holder, run) as the
	// current holder — the paused Run's checkout was already released,
	// reclaimed or never held by that run. Refusal, not retry.
	ErrNotRerouteCustodian = errors.New(
		"coord: reroute release refused — claim row does not name the asserted holder/run " +
			"(custody stays fenced §6.2/§6.3)")
	// ErrRerouteNotInProgress: the work item is not in the in_progress lane —
	// a re-route re-dispatches live work, it never resurrects terminal work or
	// double-dispatches an item already back in todo.
	ErrRerouteNotInProgress = errors.New(
		"coord: reroute release refused — work item is not in_progress")
)

// MaxHoldWindow caps how far in the future a re-route hold's resume_at may sit
// (ISI-3083, Cursor F5). resumeAt is provider-derived (now + Retry-After) and
// only rejected when zero; an absurd upstream header (a probe accepted
// now+100y) would permanently remove the item from the throttled credential's
// reachable set, and the hold has no clear/expire seam beyond its own predicate.
// The release clamps resume_at to at most MaxHoldWindow past the release
// instant, so a bad header degrades to "unavailable for MaxHoldWindow" rather
// than "forever". This mirrors resume.go capping its own backoff at BackoffCap.
const MaxHoldWindow = 6 * time.Hour

// validateCredentialKey enforces the documented "opaque identity, never token
// material" contract (§11.2 / 7.8, ISI-3083 Cursor F7) at the write boundary:
// throttledCredential lands VERBATIM in the append-only coord.audit_log (and the
// outbox), where §6.5 forbids UPDATE/DELETE — a token written here can never be
// redacted. This is a cheap SHAPE check making the contract executable: a
// credential IDENTITY (e.g. "secret:ns/name") is short and has no whitespace or
// control bytes; token material overflows the length bound or carries structure
// this rejects.
//
// ponytail: shape heuristic (length + charset), not entropy analysis — it
// rejects the obvious footguns, not a short high-entropy secret. Upgrade path: a
// distinct CredentialKey type minted at the k8s credentialSecretRef resolution
// boundary would make the identity un-constructable from raw token material.
func validateCredentialKey(field, key string) error {
	const maxLen = 253 // a k8s-ish namespace/name identity; token material overflows this
	if len(key) > maxLen {
		return fmt.Errorf("coord: %s is %d chars (> %d) — looks like token material, not an opaque identity",
			field, len(key), maxLen)
	}
	for _, r := range key {
		if r == ' ' || r < 0x20 || r == 0x7f {
			return fmt.Errorf("coord: %s contains whitespace/control characters — "+
				"credential keys are opaque identities, never token material", field)
		}
	}
	return nil
}

// RerouteReleaseResult reports the durable outcome of a release.
type RerouteReleaseResult struct {
	// Fence is the claim row's fence_token AFTER the release bump — the token
	// that fences the original holder out (every zombie write guarded on the
	// old fence or the old holder identity now fails).
	Fence int64
	// AlreadyReleased is true when a prior release for the same (item, run)
	// already landed; this call made NO change and returns the recorded
	// outcome (§6.4 idempotent re-entry — re-drives converge).
	AlreadyReleased bool
}

// rerouteEventPayload is the versioned coord.outbox body for a re-route
// release (pkg/events@rev, §10.2): correlation only, the authoritative
// custody record stays coord.claim + coord.rate_limit_reroute.
func rerouteEventPayload(throttled string, attempt int, fence int64) []byte {
	b, _ := json.Marshal(struct {
		V                   int    `json:"v"`
		Reason              string `json:"reason"`
		ThrottledCredential string `json:"throttled_credential"`
		Attempt             int    `json:"attempt"`
		Fence               int64  `json:"fence"`
	}{V: 1, Reason: "rate_limited_reroute", ThrottledCredential: throttled,
		Attempt: attempt, Fence: fence})
	return b
}

// ProdRerouteStore executes the Story 2.10 fenced release against the shipped
// coord schema (0001 + 0009). It holds no mutable Go state beyond the *sql.DB
// and the outbox flag, so its methods are safe for concurrent use by many
// goroutines (each release opens its own transaction).
type ProdRerouteStore struct {
	db         *sql.DB
	emitOutbox bool // Story 12.1: co-commit a coord.outbox event in the release txn
}

// ProdRerouteOption customises a ProdRerouteStore at construction (same
// discipline as ProdClaimerOption).
type ProdRerouteOption func(*ProdRerouteStore)

// WithRerouteOutboxCapture co-commits ONE coord.outbox domain event
// (entity=work_item, event_type=reroute_released) in the SAME transaction as
// every successful release. OFF by default for the same reason as the 2.2
// claim's option: the base spine chaos gate (0001-only) does not provision the
// outbox table; the apiserver path — which applies all migrations — enables it.
func WithRerouteOutboxCapture() ProdRerouteOption {
	return func(s *ProdRerouteStore) { s.emitOutbox = true }
}

// NewProdRerouteStore binds the re-route release to db.
func NewProdRerouteStore(db *sql.DB, opts ...ProdRerouteOption) (*ProdRerouteStore, error) {
	if db == nil {
		return nil, errors.New("coord.NewProdRerouteStore: nil db")
	}
	s := &ProdRerouteStore{db: db}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// ReleaseForReroute executes the Story 2.10 protocol step for ONE escalated
// item: fenced release of the paused Run's checkout + board re-dispatch to the
// todo lane + the throttled-credential hold — atomically, with §6.5 audit
// provenance naming the coordinator that decided the re-dispatch.
//
// Call this ONLY once the caller has established both preconditions (pure,
// caller-side): the episode escalates (ShouldReroute) AND the squad has an
// alternate credential (PickAlternateCredential). When no alternate exists the
// item STAYS PAUSED — the 2.11 resume timer owns the wake; releasing without
// an eligible claimant would just strand the item in todo.
//
// Semantics:
//
//   - (result, nil): the checkout is released at result.Fence (monotonic bump;
//     the original holder is fenced out), the item is back in the todo lane,
//     the hold records (throttledCredential, resumeAt), and the audit row +
//     optional outbox event committed with it.
//   - ({AlreadyReleased: true}, nil): a prior release for this (item, run)
//     already landed — convergent no-op, nothing written.
//   - (zero, ErrNotRerouteCustodian | ErrRerouteNotInProgress): refused; the
//     record is unchanged (transaction rolled back).
//   - (zero, err): infrastructure failure; nothing was written.
func (s *ProdRerouteStore) ReleaseForReroute(ctx context.Context, itemID, holderPrincipal, runID, throttledCredential string, attempt int, resumeAt time.Time, coordinatorPrincipal string) (RerouteReleaseResult, error) {
	if itemID == "" || holderPrincipal == "" || runID == "" || throttledCredential == "" || coordinatorPrincipal == "" {
		return RerouteReleaseResult{}, fmt.Errorf(
			"coord.ProdRerouteStore.ReleaseForReroute: itemID, holderPrincipal, runID, throttledCredential and coordinatorPrincipal are all required "+
				"(got itemID=%q holder=%q run=%q cred=%q coordinator=%q)",
			itemID, holderPrincipal, runID, throttledCredential, coordinatorPrincipal)
	}
	if attempt < 1 {
		return RerouteReleaseResult{}, fmt.Errorf(
			"coord.ProdRerouteStore.ReleaseForReroute: attempt must be >= 1 (got %d)", attempt)
	}
	if resumeAt.IsZero() {
		return RerouteReleaseResult{}, errors.New(
			"coord.ProdRerouteStore.ReleaseForReroute: resumeAt is required (the 3.7 window the hold expires at)")
	}
	// F7 (ISI-3083): the credential lands verbatim in the append-only audit log
	// and cannot be redacted — enforce the opaque-identity shape at the boundary.
	if err := validateCredentialKey("throttledCredential", throttledCredential); err != nil {
		return RerouteReleaseResult{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RerouteReleaseResult{}, fmt.Errorf("coord.ProdRerouteStore.ReleaseForReroute: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	// (0) §6.4 idempotency: a hold already naming THIS run's release is the
	// convergent outcome of a re-drive. Checked FIRST — this SELECT takes NO
	// lock and runs BEFORE the FOR UPDATE below; after a successful release the
	// claim row no longer names the holder, so the custody guard alone could not
	// recognise the re-drive, and this pre-check catches it. (A concurrent
	// re-drive that clears step 0 before either takes the row locks falls
	// through to the custody guard, which now returns ErrNotRerouteCustodian on
	// the unheld row rather than an infra NULL-scan error — see F1 below.)
	var priorFence int64
	err = tx.QueryRowContext(ctx, `
		SELECT released_fence FROM coord.rate_limit_reroute
		 WHERE work_item_id = $1::uuid AND released_run = $2::uuid`,
		itemID, runID).Scan(&priorFence)
	switch {
	case err == nil:
		return RerouteReleaseResult{Fence: priorFence, AlreadyReleased: true}, nil
	case errors.Is(err, sql.ErrNoRows):
		// no prior release for this run — continue
	default:
		return RerouteReleaseResult{}, fmt.Errorf("coord.ProdRerouteStore.ReleaseForReroute: prior hold: %w", err)
	}

	// (1) custody guard, under row locks so a concurrent §6.3 reclaim (or a
	// second releaser) serialises against us instead of interleaving.
	//
	// F1 (ISI-3083): holder_principal and run_id are NULLABLE — they are NULL on
	// every UNHELD claim row (never claimed, already released, reclaimed). They
	// MUST scan into sql.NullString: scanning NULL into a plain string surfaces
	// "converting NULL to string is unsupported" as an *infrastructure* error,
	// making both refusal sentinels unreachable — a caller that retries infra
	// failures would retry a permanent refusal forever. An unheld row is the most
	// ordinary refusal: the asserted custodian simply does not hold the checkout.
	var holder, claimRun sql.NullString
	var state string
	err = tx.QueryRowContext(ctx, `
		SELECT c.holder_principal, c.run_id::text, w.state
		  FROM coord.claim c
		  JOIN coord.work_item w ON w.id = c.work_item_id
		 WHERE c.work_item_id = $1::uuid
		   FOR UPDATE OF c, w`,
		itemID).Scan(&holder, &claimRun, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return RerouteReleaseResult{}, fmt.Errorf(
			"coord.ProdRerouteStore.ReleaseForReroute: no claim row for work item %s", itemID)
	} else if err != nil {
		return RerouteReleaseResult{}, fmt.Errorf("coord.ProdRerouteStore.ReleaseForReroute: read claim: %w", err)
	}
	if !holder.Valid || !claimRun.Valid {
		// Unheld checkout row — refusal, not infra failure.
		return RerouteReleaseResult{}, fmt.Errorf("%w: claim row is unheld (holder/run NULL), release asserts (%s, %s)",
			ErrNotRerouteCustodian, holderPrincipal, runID)
	}
	if holder.String != holderPrincipal || claimRun.String != runID {
		return RerouteReleaseResult{}, fmt.Errorf("%w: claim names (%s, %s), release asserts (%s, %s)",
			ErrNotRerouteCustodian, holder.String, claimRun.String, holderPrincipal, runID)
	}
	if state != "in_progress" {
		return RerouteReleaseResult{}, fmt.Errorf("%w: state is %s", ErrRerouteNotInProgress, state)
	}

	// (2) FENCED RELEASE — the §6.2/§6.3 discipline: bump the fence (monotonic,
	// never reused) and clear the custody columns, guarded so a concurrent
	// writer that flipped the row first makes this match 0 rows (fail closed).
	// reclaim_fenced_at is deliberately NOT stamped: that marker is 0005's
	// §6.3 reclaim protocol (pod-kill confirmation); the release evidence is
	// THIS audit row + the hold row.
	var fence int64
	err = tx.QueryRowContext(ctx, `
		UPDATE coord.claim
		   SET holder_principal = NULL,
		       run_id = NULL,
		       fence_token = fence_token + 1,
		       lease_expires_at = NULL,
		       renewed_at = NULL,
		       acquired_at = NULL,
		       initiated_by_user_id = NULL
		 WHERE work_item_id = $1::uuid
		   AND holder_principal = $2
		   AND run_id = $3::uuid
		 RETURNING fence_token`,
		itemID, holderPrincipal, runID).Scan(&fence)
	if errors.Is(err, sql.ErrNoRows) {
		return RerouteReleaseResult{}, ErrNotRerouteCustodian
	} else if err != nil {
		return RerouteReleaseResult{}, fmt.Errorf("coord.ProdRerouteStore.ReleaseForReroute: release: %w", err)
	}

	// (3) RE-DISPATCH — back to the todo lane so the §6.2 pop fans the item
	// out again. Guarded on the in_progress lane: a concurrent lane change
	// rolls the WHOLE release back (checkout and lane move are one fact — the
	// 2.2 mark discipline).
	res, err := tx.ExecContext(ctx, `
		UPDATE coord.work_item SET state = 'todo'
		 WHERE id = $1::uuid AND state = 'in_progress'`, itemID)
	if err != nil {
		return RerouteReleaseResult{}, fmt.Errorf("coord.ProdRerouteStore.ReleaseForReroute: re-dispatch: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return RerouteReleaseResult{}, ErrRerouteNotInProgress
	}

	// (4) HOLD — the throttled-credential identity and its expiry window. One
	// row per item, rewritten in place on a subsequent escalation episode (the
	// claim-row discipline; a re-route after a re-claim supersedes).
	//
	// F5 (ISI-3083): resume_at is provider-derived and otherwise unbounded — a
	// bad upstream Retry-After (a probe accepted now+100y) would strand the item
	// forever. Clamp it DB-side to at most MaxHoldWindow past the release instant
	// (clock_timestamp is authoritative; make_interval keeps the SQL bound in
	// sync with the Go constant). EXCLUDED.resume_at inherits the clamped value.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO coord.rate_limit_reroute
		       (work_item_id, throttled_credential, attempt, resume_at,
		        released_fence, released_run, coordinator)
		VALUES ($1::uuid, $2, $3,
		        LEAST($4::timestamptz, clock_timestamp() + make_interval(secs => %d)),
		        $5, $6::uuid, $7)
		ON CONFLICT (work_item_id) DO UPDATE SET
		       throttled_credential = EXCLUDED.throttled_credential,
		       attempt              = EXCLUDED.attempt,
		       resume_at            = EXCLUDED.resume_at,
		       released_fence       = EXCLUDED.released_fence,
		       released_run         = EXCLUDED.released_run,
		       coordinator          = EXCLUDED.coordinator`, int64(MaxHoldWindow.Seconds())),
		itemID, throttledCredential, attempt, resumeAt, fence, runID, coordinatorPrincipal); err != nil {
		return RerouteReleaseResult{}, fmt.Errorf("coord.ProdRerouteStore.ReleaseForReroute: hold: %w", err)
	}

	// (5) §6.5 provenance — one claim_released row naming the coordinator that
	// decided the re-dispatch, the run released, the fence installed, and the
	// throttled credential the hold pins (the operator-facing "why").
	payload, err := json.Marshal(map[string]any{
		"reason":               "rate_limited_reroute",
		"throttled_credential": throttledCredential,
		"attempt":              attempt,
		"resume_at":            resumeAt.UTC().Format(time.RFC3339Nano),
		"released_fence":       fence,
	})
	if err != nil {
		return RerouteReleaseResult{}, fmt.Errorf("coord.ProdRerouteStore.ReleaseForReroute: audit payload: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal, fence_token,
		        from_state, to_state, payload)
		VALUES ($1::uuid, $2::uuid, 'claim_released', $3, $4, 'in_progress', 'todo', $5::jsonb)`,
		itemID, runID, coordinatorPrincipal, fence, string(payload)); err != nil {
		return RerouteReleaseResult{}, fmt.Errorf("coord.ProdRerouteStore.ReleaseForReroute: audit: %w", err)
	}

	// (5b) Story 12.1 domain-event capture, same transaction (opt-in — see
	// WithRerouteOutboxCapture).
	if s.emitOutbox {
		if err := events.CaptureForWorkItem(ctx, tx, itemID, runID,
			"reroute_released", rerouteEventPayload(throttledCredential, attempt, fence)); err != nil {
			return RerouteReleaseResult{}, fmt.Errorf("coord.ProdRerouteStore.ReleaseForReroute: outbox: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return RerouteReleaseResult{}, fmt.Errorf("coord.ProdRerouteStore.ReleaseForReroute: commit: %w", err)
	}
	return RerouteReleaseResult{Fence: fence}, nil
}

// ---------------------------------------------------------------------------
// Claim: the credentialed §6.2 acquire (ProdClaimer extension)
// ---------------------------------------------------------------------------

// rerouteHoldGuard is the different-credential predicate appended to the claim
// statements: while a LIVE hold (resume_at still future) pins credentialKey on
// this item, the claim is refused. It is the 7.6 attribution enforcing the 2.10
// routing intent — the re-routed item goes to an Agent presenting its OWN,
// DIFFERENT credential, never back to the throttled one before the window
// clears.
const rerouteHoldGuard = `
		   AND NOT EXISTS (
		       SELECT 1 FROM coord.rate_limit_reroute h
		        WHERE h.work_item_id = %s
		          AND h.throttled_credential = $%d
		          AND h.resume_at > clock_timestamp())`

// ClaimNextCredentialed is ProdClaimer.ClaimNext with the Story 2.10
// different-credential guard: identical §6.2 fan-out transaction (SKIP LOCKED
// pop → guarded acquire → board advance → §6.5 audit), except items with a
// LIVE re-route hold pinning credentialKey are INVISIBLE to the pop — the
// claimant only ever receives work its own credential may run (7.6). Items
// with no hold, or whose hold's window has cleared, claim as usual (the 3.7
// auto-resume branch).
//
// credentialKey is the claimant Agent's OWN credential identity (caller-side
// resolved from its spec.credentialSecretRef — the seat-model §11.2 invariant:
// an Agent never presents a foreign credential, so passing a key != the
// Agent's own is a caller defect this layer cannot detect).
func (p *ProdClaimer) ClaimNextCredentialed(ctx context.Context, principal, runID, credentialKey, initiatedByUserID string) (itemID string, fence int64, ok bool, err error) {
	// F3 (ISI-3083): the sibling precondition ClaimNext and
	// AcquireSpecificCredentialed both enforce. Without it an empty principal
	// claims with holder_principal = '' (non-NULL), whose free-or-expired guard
	// then blocks every real claimant until the lease lapses, and audits the
	// claim to principal = ''.
	if principal == "" || runID == "" {
		return "", 0, false, fmt.Errorf(
			"coord.ProdClaimer.ClaimNextCredentialed: principal and runID are required (got principal=%q runID=%q)",
			principal, runID)
	}
	if credentialKey == "" {
		return "", 0, false, errors.New(
			"coord.ProdClaimer.ClaimNextCredentialed: credentialKey is required (the claimant's own credential identity, 7.6)")
	}
	// F7 (ISI-3083): the credential key is compared in the hold guard and is the
	// same opaque-identity contract enforced at the release boundary.
	if err := validateCredentialKey("credentialKey", credentialKey); err != nil {
		return "", 0, false, err
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, false, fmt.Errorf("coord.ProdClaimer.ClaimNextCredentialed: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	// (1) pop — the 2.2 statement plus the live-hold guard ($2 = credential).
	pick := `
		SELECT w.id
		  FROM coord.work_item w
		  JOIN coord.claim c ON c.work_item_id = w.id
		 WHERE w.state = $1
		   AND (c.holder_principal IS NULL OR c.lease_expires_at < clock_timestamp())` +
		fmt.Sprintf(rerouteHoldGuard, "w.id", 2) + `
		 ORDER BY w.created_at, w.id
		 LIMIT 1
		 FOR UPDATE OF w SKIP LOCKED`
	var id string
	switch err := tx.QueryRowContext(ctx, pick, p.cfg.ClaimableState, credentialKey).Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		return "", 0, false, nil // exhausted, contended, or every candidate is held for this credential
	case err != nil:
		return "", 0, false, fmt.Errorf("coord.ProdClaimer.ClaimNextCredentialed: pick: %w", err)
	}

	fence, ok, err = p.acquireCredentialed(ctx, tx, principal, runID, id, credentialKey, initiatedByUserID)
	if err != nil || !ok {
		return "", 0, false, err
	}
	return id, fence, true, nil
}

// AcquireSpecificCredentialed is ProdClaimer.AcquireSpecific with the Story
// 2.10 different-credential guard: the §6.2 guarded acquire of a SPECIFIC
// item, refused (ok=false, nothing changed) while a LIVE re-route hold pins
// credentialKey on it. This is the by-ID counterpart the 2.9 coordinator
// dispatch consumer uses — the "other Agent claims it" step when the claim is
// directed rather than popped.
func (p *ProdClaimer) AcquireSpecificCredentialed(ctx context.Context, principal, runID, itemID, credentialKey, initiatedByUserID string) (string, int64, bool, error) {
	if principal == "" || runID == "" || itemID == "" {
		return "", 0, false, fmt.Errorf(
			"coord.ProdClaimer.AcquireSpecificCredentialed: principal, runID and itemID are required (got principal=%q runID=%q itemID=%q)",
			principal, runID, itemID)
	}
	if credentialKey == "" {
		return "", 0, false, errors.New(
			"coord.ProdClaimer.AcquireSpecificCredentialed: credentialKey is required (the claimant's own credential identity, 7.6)")
	}
	// F7 (ISI-3083): same opaque-identity shape contract as the release boundary.
	if err := validateCredentialKey("credentialKey", credentialKey); err != nil {
		return "", 0, false, err
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, false, fmt.Errorf("coord.ProdClaimer.AcquireSpecificCredentialed: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	fence, ok, err := p.acquireCredentialed(ctx, tx, principal, runID, itemID, credentialKey, initiatedByUserID)
	if err != nil || !ok {
		return "", 0, false, err
	}
	return itemID, fence, true, nil
}

// acquireCredentialed runs the shared tail of both credentialed claims inside
// tx: guarded in-place acquire (fence bump + lease, plus the live-hold
// credential guard) → board advance from the claimable lane → §6.5
// claim_acquired audit → optional 12.1 outbox capture. On the happy path it
// COMMITS tx itself (F10/ISI-3083 doc fix — the prior comment wrongly said the
// caller commits); on a guard rejection or lane race it Rolls tx back and
// returns (0, false, nil); on error it leaves the caller's deferred Rollback to
// fire. Either way the public method must not touch tx after this returns.
func (p *ProdClaimer) acquireCredentialed(ctx context.Context, tx *sql.Tx, principal, runID, itemID, credentialKey, initiatedByUserID string) (int64, bool, error) {
	// The 2.2 acquire statement (fence bump, live wall-clock lease) plus the
	// live-hold guard ($5 = credential), RETURNING the fresh fence. The UPDATE
	// target is ALIASED (c) so the guard's correlated subquery resolves
	// c.work_item_id unambiguously.
	acq := fmt.Sprintf(`
		UPDATE coord.claim AS c
		   SET holder_principal = $1,
		       run_id = $2::uuid,
		       fence_token = fence_token + 1,
		       lease_expires_at = clock_timestamp() + interval '%s',
		       acquired_at = clock_timestamp(),
		       renewed_at = NULL,
		       initiated_by_user_id = $3::uuid
		 WHERE c.work_item_id = $4::uuid
		   AND (c.holder_principal IS NULL OR c.lease_expires_at < clock_timestamp())`,
		p.cfg.LeaseInterval) + fmt.Sprintf(rerouteHoldGuard, "c.work_item_id", 5) + `
		 RETURNING fence_token`

	var initiator any
	if initiatedByUserID != "" {
		initiator = initiatedByUserID
	}
	var fence int64
	switch err := tx.QueryRowContext(ctx, acq, principal, runID, initiator, itemID, credentialKey).Scan(&fence); {
	case errors.Is(err, sql.ErrNoRows):
		// Guard rejected us: live foreign lease, or a LIVE hold pins this
		// claimant's credential on the item. Nothing changed.
		_ = tx.Rollback()
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("coord.ProdClaimer.acquireCredentialed: acquire: %w", err)
	}

	// Board advance, atomic with the acquire (the 2.2 mark discipline).
	res, err := tx.ExecContext(ctx, p.mark, p.cfg.ClaimedState, itemID, p.cfg.ClaimableState)
	if err != nil {
		return 0, false, fmt.Errorf("coord.ProdClaimer.acquireCredentialed: mark claimed: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// Left the claimable lane concurrently: roll the whole claim back.
		_ = tx.Rollback()
		return 0, false, nil
	}

	if _, err := tx.ExecContext(ctx, p.audit, itemID, runID, principal, initiator, fence,
		p.cfg.ClaimableState, p.cfg.ClaimedState); err != nil {
		return 0, false, fmt.Errorf("coord.ProdClaimer.acquireCredentialed: audit: %w", err)
	}
	if err := p.captureClaim(ctx, tx, itemID, runID, principal, fence); err != nil {
		return 0, false, fmt.Errorf("coord.ProdClaimer.acquireCredentialed: outbox: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("coord.ProdClaimer.acquireCredentialed: commit: %w", err)
	}
	return fence, true, nil
}
