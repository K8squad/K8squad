// credsignal.go — the credentialLifecycle signal contract and its applier
// (Stories 7.4+7.6 / ISI-2898, arch §10, §7.2, FR-G3).
//
// The SHIM (Epic 5.10, not yet landed) surfaces credential lifecycle events
// from inside the running agent container — the token stopped authenticating,
// the provider 429'd with a Retry-After, the Secret rotated under the mount,
// the BYO endpoint stopped answering. CredentialSignal is the typed contract
// that side of the seam will call; ApplyCredentialSignal is the control-plane
// half that lands TODAY and is what those signals will plug into unchanged.
//
// One ApplyCredentialSignal is ONE transaction co-committing the three facts
// that must never diverge (the §6.4/AC6 discipline shared with
// ProdReconcileStore.Advance):
//
//	INSERT/UPDATE coord.credential_pause — the per-credential attributed episode (7.6)
//	UPDATE coord.claim SET reconcile_step = 'paused(<reason>)'  — the durable Run hold (7.4)
//	INSERT INTO coord.outbox ('run','paused',…) — the operator signal (FR-F6 / 8.6/8.11)
//
// The step move is GUARDED (WHERE reconcile_step IN the live set): a signal for
// a Run that already reached a terminal step, or that is racing a concurrent
// pause, commits the ledger attribution but moves nothing — an expired-token
// event must never resurrect a finished Run. The outbox row carries the
// attribution payload (reason, credential, principal, resume_at) so the console
// renders "paused: rate-limited (credential X, resumes ~T)" from the event
// without a ledger join.
package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/K8squad/K8squad/pkg/reconcile"
)

// CredentialEvent is the lifecycle event kind the shim observes (§7.2
// credentialLifecycle). It maps 1:1 onto the reason family; kept distinct from
// CredPauseReason so the wire contract and the ledger enum can evolve apart.
type CredentialEvent string

const (
	EventExpired      CredentialEvent = "expired"       // token no longer authenticates
	EventRotated      CredentialEvent = "rotated"       // Secret rotated mid-Run
	EventRateLimited  CredentialEvent = "rate_limited"  // provider throttle, Retry-After present
	EventUnreachable  CredentialEvent = "unreachable"   // BYO endpoint not answering (7.5)
)

// SignalStep maps the observed event onto the durable pause step (7.4): the
// same pause/resume family, a distinct legible reason per hold. Fail-closed —
// an unknown event maps to "" and ApplyCredentialSignal rejects it.
func SignalStep(e CredentialEvent) reconcile.Step {
	switch e {
	case EventRateLimited:
		return reconcile.StepPausedRateLimited
	case EventExpired:
		return reconcile.StepPausedCredentialExpired
	case EventRotated:
		return reconcile.StepPausedCredentialRotated
	case EventUnreachable:
		return reconcile.StepPausedEndpointUnreachable
	default:
		return ""
	}
}

// EventReason is the inverse mapping (step → ledger reason) used when the
// reconciler re-derives the ledger view for the status projection.
func EventReason(e CredentialEvent) CredPauseReason {
	switch e {
	case EventRateLimited:
		return ReasonRateLimited
	case EventExpired:
		return ReasonCredentialExpired
	case EventRotated:
		return ReasonCredentialRotated
	case EventUnreachable:
		return ReasonEndpointUnreachable
	default:
		return ""
	}
}

// CredentialSignal is ONE credentialLifecycle observation from the shim (5.10):
// which credential, on which Run, what happened, and (rate limits only) the
// provider's Retry-After window. Run and Item are the provenance — the
// ATTRIBUTION key is the credential (7.6), so two Runs sharing a seat throttle
// together, correctly.
type CredentialSignal struct {
	Run        string           // run_id (required: the Run being held)
	Item       string           // work_item_id (the Run's item; "" ⇒ NULL)
	Credential CredentialRef    // the per-user credential that surfaced the event
	Event      CredentialEvent  // expired | rotated | rate_limited | unreachable
	RetryAfter *time.Duration   // rate_limited only
}

// validate enforces the contract before any DB work (fail-closed).
func (s CredentialSignal) validate() error {
	if s.Run == "" {
		return errors.New("coord.CredentialSignal: Run is required (the signal holds a Run)")
	}
	if s.Credential.ID == "" {
		return errors.New("coord.CredentialSignal: Credential.ID is required (attribution is the point, 7.6)")
	}
	if !s.Credential.Class.Valid() {
		return fmt.Errorf("coord.CredentialSignal: invalid credential class %q", s.Credential.Class)
	}
	if SignalStep(s.Event) == "" {
		return fmt.Errorf("coord.CredentialSignal: unknown event %q", s.Event)
	}
	if s.Event != EventRateLimited && s.RetryAfter != nil {
		return fmt.Errorf("coord.CredentialSignal: Retry-After applies to rate_limited only (got %s)", s.Event)
	}
	return nil
}

// ApplyResult reports what one applied signal did: the attributed episode, and
// whether the durable step actually moved (false ⇔ the Run was not in a
// pausable state — terminal, or already paused for this same reason — while
// the ledger attribution still committed).
type ApplyResult struct {
	Episode  CredPauseInfo
	StepMoved bool
}

// pausedFromSteps is the live set a credential signal may hold. Terminal steps
// are excluded (an expired-token event must never resurrect a finished Run);
// the non-terminal steps are included, so the NEWEST signal wins — a
// credential that expired while rate-limited is now expired — and a re-write
// of the identical step value is a durable no-op while the ledger refresh
// (new Retry-After, attempt streak kept) still commits.
var pausedFromSteps = []reconcile.Step{
	reconcile.StepRunning,
	reconcile.StepDispatching,
	reconcile.StepCollecting,
	reconcile.StepPaused,
	reconcile.StepPausedRateLimited,
	reconcile.StepPausedCredentialExpired,
	reconcile.StepPausedCredentialRotated,
	reconcile.StepPausedEndpointUnreachable,
}

// ApplyCredentialSignal lands one shim signal atomically: attributed
// per-credential episode + guarded durable step move + outbox operator event,
// all in ONE transaction (see the file header). It is the control-plane
// consumer of the 5.10 contract and the ONLY writer that moves a Run INTO the
// credential pause family; exits are ResumeOnRefresh (refresh-mode) and the
// Timer over ResumeDue (rate_limited), both of which re-enter via the
// reconciler's normal Claiming/Running edges.
//
// principal identifies the actor driving the apply (the shim service identity)
// for the co-committed audit row; projectID rides the outbox subject taxonomy
// (§17.4) and is required by the events seam.
func ApplyCredentialSignal(ctx context.Context, db *sql.DB, cfg CredPauseConfig, principal, projectID string, sig CredentialSignal) (ApplyResult, error) {
	if db == nil {
		return ApplyResult{}, errors.New("coord.ApplyCredentialSignal: nil db")
	}
	if err := cfg.validate(); err != nil {
		return ApplyResult{}, err
	}
	if err := sig.validate(); err != nil {
		return ApplyResult{}, err
	}
	if principal == "" {
		return ApplyResult{}, errors.New("coord.ApplyCredentialSignal: principal is required (§6.5 provenance)")
	}
	if projectID == "" {
		return ApplyResult{}, errors.New("coord.ApplyCredentialSignal: projectID is required (outbox subject taxonomy, §17.4)")
	}

	store := &CredentialPauseStore{db: db, cfg: cfg, rand: func() float64 { return randv2() }}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("coord.ApplyCredentialSignal: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1 — the attributed episode (same planning as PauseCredential, inside this tx).
	epi, err := store.pauseCredentialTx(ctx, tx, sig)
	if err != nil {
		return ApplyResult{}, err
	}

	// 2 — the guarded durable step move (AC6 discipline: co-committed, never diverging).
	step := SignalStep(sig.Event)
	q := `UPDATE coord.claim SET reconcile_step = $1
		  WHERE work_item_id = $2 AND reconcile_step = ANY($3)`
	var item any
	if sig.Item != "" {
		item = sig.Item
	}
	res, err := tx.ExecContext(ctx, q, string(step), item, stepList(pausedFromSteps))
	if err != nil {
		return ApplyResult{}, fmt.Errorf("coord.ApplyCredentialSignal: step move: %w", err)
	}
	moved, err := res.RowsAffected()
	if err != nil {
		return ApplyResult{}, fmt.Errorf("coord.ApplyCredentialSignal: step rows: %w", err)
	}

	// 3 — the operator signal (§6.6 canonical outbox; the console's 8.6/8.11 string).
	payload := map[string]any{
		"reason":         string(EventReason(sig.Event)),
		"credential_id":  sig.Credential.ID,
		"principal":      sig.Credential.Principal,
		"credential_class": string(sig.Credential.Class),
		"run_id":         sig.Run,
	}
	if epi.ResumeAt != nil {
		payload["resume_at"] = epi.ResumeAt.UTC().Format(time.RFC3339)
	} else {
		payload["resume_mode"] = "refresh"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("coord.ApplyCredentialSignal: payload: %w", err)
	}
	var squad any
	var wid any
	if sig.Item != "" {
		wid = sig.Item
	}
	ev := `INSERT INTO coord.outbox
		     (entity, project_id, squad, event_type, work_item_id, run_id, payload)
		 VALUES ('run', $1, $2, 'paused', $3, $4, $5)`
	if _, err := tx.ExecContext(ctx, ev, projectID, squad, wid, sig.Run, body); err != nil {
		return ApplyResult{}, fmt.Errorf("coord.ApplyCredentialSignal: outbox: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return ApplyResult{}, fmt.Errorf("coord.ApplyCredentialSignal: commit: %w", err)
	}
	return ApplyResult{Episode: epi, StepMoved: moved > 0}, nil
}

// stepList renders the live set as a Postgres array literal parameter.
func stepList(steps []reconcile.Step) any {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = string(s)
	}
	return out
}

// pauseCredentialTx is PauseCredential's planning + write against a CALLER-OWNED
// transaction (the ApplyCredentialSignal co-commit path). Same semantics; the
// tx never commits here.
func (s *CredentialPauseStore) pauseCredentialTx(ctx context.Context, tx *sql.Tx, sig CredentialSignal) (CredPauseInfo, error) {
	reason := EventReason(sig.Event)

	var priorAttempt sql.NullInt32
	var priorResumedAt sql.NullTime
	read := fmt.Sprintf(
		`SELECT attempt, resumed_at FROM %s WHERE credential_id=$1 FOR UPDATE`, s.cfg.Pause)
	switch err := tx.QueryRowContext(ctx, read, sig.Credential.ID).Scan(&priorAttempt, &priorResumedAt); err {
	case nil, sql.ErrNoRows:
		// new episode below
	default:
		return CredPauseInfo{}, fmt.Errorf("coord.ApplyCredentialSignal: read prior: %w", err)
	}

	now := dbNow(ctx, tx)
	attempt := planAttempt(priorAttempt, priorResumedAt, now, s.cfg.BackoffReset)

	var resumeAt *time.Time
	var retryAfter *time.Duration
	if reason.TimerResumed() {
		if sig.RetryAfter != nil && *sig.RetryAfter > 0 {
			retryAfter = sig.RetryAfter
		} else {
			d := EqualJitter(s.cfg.BackoffBase, s.cfg.BackoffCap, attempt, s.rand())
			retryAfter = &d
		}
		at := now.Add(*retryAfter)
		resumeAt = &at
	}

	var item, run any
	if sig.Item != "" {
		item = sig.Item
	}
	if sig.Run != "" {
		run = sig.Run
	}
	var raMs any
	if reason.TimerResumed() {
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
		sig.Credential.ID, sig.Credential.Principal, string(sig.Credential.Class), string(reason),
		item, run, raMs, attempt, resumeAt, now); err != nil {
		return CredPauseInfo{}, fmt.Errorf("coord.ApplyCredentialSignal: write: %w", err)
	}
	return CredPauseInfo{Reason: reason, Attempt: attempt, ResumeAt: resumeAt, RetryAfter: retryAfter}, nil
}
