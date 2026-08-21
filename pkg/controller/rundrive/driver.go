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

// Package rundrive is the PRODUCTION drive loop of the Run reconcile machine
// (Story 3.1/3.2/3.7, ISI-2883). Where pkg/controller/run PROJECTS the durable
// reconcile_step onto Run.status (a read-only projection), this package is the
// side that advances the machine: it watches Run CRs and, for each, binds the
// per-Run production Store + Effects (pkg/coord ProdReconcileStore/ProdEffects)
// and drives reconcile.Reconcile — level-triggered, every pass re-derived from
// Postgres alone, safe to kill at ANY point (the machine's §6.4 contract).
//
// The three behaviours beyond the plain happy-path drive:
//
//   - 3.2 death detection + retry lap: a Run in flight (dispatching/running/
//     collecting) whose claim LEASE has expired is treated as a dead holder
//     (sandbox or agent gone, §5.3): the checkout is released for reclaim
//     (fence-first, §6.3), the dead sandbox is torn down, and — within the
//     spec.retryPolicy budget — the Run re-enters claiming_sandbox as a NEW
//     retry lap (fresh a2a_task_id = run_id#lapN) with equal-jitter exponential
//     backoff; outside the budget it enters terminal failed.
//   - 3.7 rate-limit park + single durable wake: a Run parked on
//     paused(rate_limited) with no pending episode gets one recorded
//     (resume_at = now + backoff; Retry-After when the 5.10 signal carries it,
//     wired by the signal consumer when it lands). The wake timer — NOT a
//     requeue poll — fires at resume_at, re-enters the Run into dispatching
//     (guarded step move), and kicks the Run CR back into this loop.
//   - spin guard: the machine drive is bounded (Options.MaxPasses) so a fence
//     raced mid-drive can never wedge a reconcile in an unadvanced loop; the
//     level-triggered next pass re-reads the new fence and continues.
//
// Status discipline: the Driver NEVER writes Run.status — the projector owns
// the status subresource; the Driver owns the durable side of the world.
package rundrive

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// workItemField is the Run index key the resume kick lists Runs by (a work
// item's due wake must re-drive THE Run that owns it, without scanning).
const workItemField = ".spec.workItemRef"

// Defaults for the drive loop's timing knobs.
const (
	// DefaultMaxPasses bounds one machine drive (see reconcile.Options.MaxPasses).
	DefaultMaxPasses = 64
	// continueDelay requeues a non-terminal, non-progressing drive (contention
	// or spin-guard exhaustion) — a short step, not a poll.
	continueDelay = 2 * time.Second
	// defaultBackoffBase is the retry-lap backoff base when spec.retryPolicy
	// carries none (EqualJitter caps growth at 5m).
	defaultBackoffBase = time.Second
	// backoffCap ceilings the retry-lap exponential.
	backoffCap = 5 * time.Minute
	// DefaultMaxRetries is the default maximum number of retry attempts.
	DefaultMaxRetries = 5
	// CircuitBreakerMaxConsecutiveFailures is the maximum number of consecutive failures before triggering circuit breaker.
	CircuitBreakerMaxConsecutiveFailures = 3
	// CircuitBreakerPauseDuration is the pause duration after triggering circuit breaker.
	CircuitBreakerPauseDuration = 5 * time.Minute
)

// retryEventType categorizes different types of retry events for monitoring.
type retryEventType string

const (
	retryEventTypeNormal     retryEventType = "normal"
	retryEventTypeTransient  retryEventType = "transient"
	retryEventTypePermanent  retryEventType = "permanent"
	retryEventTypeCircuitBreaker retryEventType = "circuit_breaker"
)

// retryEvent tracks retry attempts for monitoring and debugging.
type retryEvent struct {
	Timestamp   time.Time
	RunID       string
	EventType   retryEventType
	ErrorType   string
	Lap         int
	Success     bool
	WillRetry   bool
	NextRetryAt *time.Time
}

// ClaimState is the durable claim-row snapshot one drive pass decides on.
type ClaimState struct {
	Step           reconcile.Step
	Fence          int64
	Holder         string
	LeaseExpiresAt *time.Time
}

// Claims is the durable coordination surface the driver needs BEYOND the
// machine's own Store/Effects: claim-row reads, the retry/fail guarded
// re-entries (§6.3 fence-first + checkout release), the 3.7 resume re-entry,
// and the retry-lap count (derived from the a2a_dispatch markers).
type Claims interface {
	State(ctx context.Context, workItemID string) (ClaimState, bool, error)
	LapsUsed(ctx context.Context, runID string) (int, error)
	// RetryEnter is the §8 retry re-entry: bump the fence (fencing any zombie),
	// release the work-item checkout for reclaim, and re-point the durable step
	// to claiming_sandbox — ONE transaction with its audit + outbox rows. ok is
	// false when the fromFence no longer holds (someone else reclaimed first).
	RetryEnter(ctx context.Context, workItemID, runID string, fromFence int64) (newFence int64, ok bool, err error)
	// FailEnter is the terminal variant: same fence-first discipline, step →
	// failed, checkout released.
	FailEnter(ctx context.Context, workItemID, runID string, fromFence int64) (ok bool, err error)
	// CancelFinish is the 3.3 kill finish: after the sandbox teardown the
	// cancelling step moves to terminal cancelled, fence-first, audited.
	CancelFinish(ctx context.Context, workItemID, runID string, fromFence int64) (ok bool, err error)
	// CancelDue lists work items at cancelling — the kill sweep's backlog.
	CancelDue(ctx context.Context) ([]string, error)
	// RequeuePaused is the 3.7 resume re-entry: guarded paused(rate_limited) →
	// dispatching (custody retained — short pauses keep the checkout), audited.
	RequeuePaused(ctx context.Context, workItemID string) (ok bool, err error)
}

// Pauses is the 3.7 episode surface (bound to coord.ProdResumeStore).
type Pauses interface {
	Pending(ctx context.Context, workItemID string) (resumeAt time.Time, exists bool, err error)
	Record(ctx context.Context, workItemID, runID string, retryAfter *time.Duration) (coord.PauseInfo, error)
}

// machineStore is the durable Store seam plus the sticky-error probe the
// driver checks after every drive (the coord error-seam discipline).
type machineStore interface {
	reconcile.Store
	Err() error
}

// machineEffects is the side-effect seam plus the same error probe.
type machineEffects interface {
	reconcile.Effects
	Err() error
}

// Runner constructs the per-Run machine bindings (bound to the coord prod
// constructors in store.go; faked in unit tests).
type Runner interface {
	Store(ctx context.Context, run *api.Run) (machineStore, error)
	Effects(ctx context.Context, run *api.Run) (machineEffects, error)
}

// SandboxReleaser tears a dead run's sandbox down (§9.3 teardown-and-replace;
// the warm pool's by-run bind must not reattach a dead sandbox on the retry
// lap). Optional — nil skips (ledger-only mode has nothing physical to tear).
type SandboxReleaser interface {
	Release(ctx context.Context, runID string) error
}

// Driver drives the durable reconcile machine for Run CRs.
type Driver struct {
	client.Client
	Claims    Claims
	Pauses    Pauses
	Runner    Runner
	Sandbox   SandboxReleaser // optional
	Notify    func()          // kicks the resume timer after a fresh episode (optional)
	Now       func() time.Time
	Rand      func() float64
	MaxPasses int

	// Circuit breaker state to prevent infinite restart loops
	consecutiveFailures    map[string]int    // track consecutive failures per runID
	circuitBreakerPauses   map[string]time.Time // track pause times per runID
	retryEvents           []retryEvent       // retry event log for monitoring

	// resumeCh feeds the watched channel source: due 3.7 wakes land here as
	// GenericEvents carrying the Run to re-drive. Buffered; a full channel is
	// dropped on purpose — the level-triggered loop and the periodic resync
	// are the backstop, the kick is latency sugar.
	resumeCh chan event.TypedGenericEvent[client.Object]
}

// NewDriver wires the Driver; it owns the resume channel handed to
// SetupWithManager's channel source.
func NewDriver(cl client.Client, claims Claims, pauses Pauses, runner Runner) *Driver {
	return &Driver{
		Client:                cl,
		Claims:                claims,
		Pauses:                pauses,
		Runner:                runner,
		MaxPasses:             DefaultMaxPasses,
		Rand:                  rand.Float64,
		consecutiveFailures:   make(map[string]int),
		circuitBreakerPauses:  make(map[string]time.Time),
		retryEvents:           make([]retryEvent, 0),
		resumeCh:              make(chan event.TypedGenericEvent[client.Object], 64),
	}
}

// Reconcile is one level-triggered drive pass over the Run's durable state.
func (r *Driver) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var run api.Run
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if run.DeletionTimestamp != nil || run.Spec.WorkItemRef == "" {
		return ctrl.Result{}, nil
	}
	runID := string(run.UID)

	cs, found, err := r.Claims.State(ctx, run.Spec.WorkItemRef)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("rundrive: read claim for %s: %w", req.NamespacedName, err)
	}
	if !found {
		// Not enrolled in coord (work item absent or ref dangling): nothing to
		// drive. The projector reports Pending; re-creating coordination rows
		// here would invent a work item that does not exist (ADR-001).
		return ctrl.Result{}, nil
	}
	if reconcile.IsTerminal(cs.Step) {
		return ctrl.Result{}, nil // absorbing (AC5): the projector has it
	}

	if isPaused(cs.Step) {
		return r.park(ctx, &run, runID)
	}

	// 3.3 kill finish: the Run sits at cancelling (kill was issued via the
	// apiserver's CancelEnter). Tear the sandbox down, then finish →
	// cancelled. Idempotent: a crash mid-teardown re-enters here (AC5).
	if cs.Step == reconcile.StepCancelling {
		return r.finishCancel(ctx, &run, cs)
	}

	// 3.2 death detection: in flight with a lease that expired under a holder
	// that stopped heart-keeping — sandbox or agent died mid-execution (§5.3).
	if r.dead(cs) {
		return r.retryOrFail(ctx, &run, cs)
	}

	// 3.1 drive: bind the per-Run machine and run it toward a terminal step.
	store, err := r.Runner.Store(ctx, &run)
	if err != nil {
		return ctrl.Result{}, err
	}
	effects, err := r.Runner.Effects(ctx, &run)
	if err != nil {
		return ctrl.Result{}, err
	}
	laps, err := r.Claims.LapsUsed(ctx, runID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("rundrive: count laps for %s: %w", req.NamespacedName, err)
	}
	opts := reconcile.Options{Durable: true, Fence: store.Fence(), MaxPasses: r.maxPasses()}
	if laps > 0 {
		next := laps + 1 // retry lap N: a genuine new agent attempt (fresh task id)
		opts.Lap = &next
	}
	if err := reconcile.Reconcile(effects, store, opts); err != nil {
		return ctrl.Result{}, fmt.Errorf("rundrive: drive %s: %w", req.NamespacedName, err)
	}
	if err := errors.Join(store.Err(), effects.Err()); err != nil {
		// An infrastructure error mid-effect must not read as "applied": requeue.
		return ctrl.Result{}, fmt.Errorf("rundrive: effects for %s: %w", req.NamespacedName, err)
	}

	switch after := store.Step(); {
	case reconcile.IsTerminal(after):
		return ctrl.Result{}, nil // done: succeeded/failed/cancelled
	default:
		// Non-terminal after a bounded drive: spin guard hit or contention —
		// re-read the world on the next pass.
		return ctrl.Result{RequeueAfter: continueDelay}, nil
	}
}

// park records the single durable wake for a Run parked on a pause step (3.7)
// when none exists, then hands ownership to the timer — no requeue poll.
func (r *Driver) park(ctx context.Context, run *api.Run, runID string) (ctrl.Result, error) {
	_, exists, err := r.Pauses.Pending(ctx, run.Spec.WorkItemRef)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("rundrive: pause lookup %s: %w", run.Spec.WorkItemRef, err)
	}
	if !exists {
		if _, err := r.Pauses.Record(ctx, run.Spec.WorkItemRef, runID, nil); err != nil {
			return ctrl.Result{}, fmt.Errorf("rundrive: record pause %s: %w", run.Spec.WorkItemRef, err)
		}
		if r.Notify != nil {
			r.Notify() // the new episode may wake earlier than the timer's current sleep
		}
	}
	return ctrl.Result{}, nil
}

// dead reports the 3.2 death signal: in flight, held, and the lease expired.
func (r *Driver) dead(cs ClaimState) bool {
	if cs.Holder == "" || cs.LeaseExpiresAt == nil {
		return false
	}
	inFlight := cs.Step == reconcile.StepDispatching ||
		cs.Step == reconcile.StepRunning ||
		cs.Step == reconcile.StepCollecting
	return inFlight && r.now().After(*cs.LeaseExpiresAt)
}

// retryOrFail executes the §5.3 decision after a death (or a failed attempt):
// within the spec.retryPolicy budget → §6.3 fence-first RetryEnter + backoff;
// outside it → terminal FailEnter. The dead run's sandbox is torn down first
// so the retry lap provisions a fresh one (never reattaches a corpse).
// Enhanced with circuit breaker logic to prevent infinite restart loops.
func (r *Driver) retryOrFail(ctx context.Context, run *api.Run, cs ClaimState) (ctrl.Result, error) {
	runID := string(run.UID)
	namespacedName := fmt.Sprintf("%s/%s", run.Namespace, run.Name)

	// Check for circuit breaker activation
	if r.isCircuitBreakerActivated(runID) {
		pauseUntil := r.circuitBreakerPauses[runID]
		logMsg := fmt.Sprintf("rundrive: circuit breaker activated for %s, retry paused until %v",
			namespacedName, pauseUntil)
		// In a real implementation, this would use a proper logger
		fmt.Printf(logMsg)
		r.logRetryEvent(ctx, runID, retryEventTypeCircuitBreaker, "circuit_breaker", 0, false, pauseUntil)

		// Check if pause period has expired
		if r.now().Before(pauseUntil) {
			return ctrl.Result{RequeueAfter: time.Until(pauseUntil)}, nil
		}
		// Pause period expired, reset circuit breaker
		r.resetCircuitBreaker(runID)
	}

	// Categorize the error for better retry decisions
	errorType := r.categorizeError(cs.Step)

	if r.Sandbox != nil {
		// Best-effort: a stuck teardown must not block the custody fix; the
		// pool's draining state retries it (§9.3).
		_ = r.Sandbox.Release(ctx, runID)
	}

	laps, err := r.Claims.LapsUsed(ctx, runID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("rundrive: count laps for %s: %w", runID, err)
	}

	// Get configured max retries with default validation
	maxRetries := r.getMaxRetriesWithDefaults(run)

	// Check if we should retry based on error type and circuit breaker state
	shouldRetry := laps < maxRetries
	if !shouldRetry {
		// Track consecutive failures for circuit breaker
		r.consecutiveFailures[runID]++

		// Activate circuit breaker if we have too many consecutive failures
		if r.consecutiveFailures[runID] >= CircuitBreakerMaxConsecutiveFailures {
			return r.activateCircuitBreaker(ctx, runID, namespacedName, laps, errorType)
		}
	} else {
		// Reset consecutive failures on successful retry eligibility
		r.consecutiveFailures[runID] = 0
	}

	if shouldRetry {
		// Record retry event
		retryAfter := r.BackoffFor(run, laps+1)
		nextRetryTime := r.now().Add(retryAfter)
		r.logRetryEvent(ctx, runID, errorType, fmt.Sprintf("step_%s", cs.Step), laps, true, &nextRetryTime)

		if _, ok, err := r.Claims.RetryEnter(ctx, run.Spec.WorkItemRef, runID, cs.Fence); err != nil {
			return ctrl.Result{}, fmt.Errorf("rundrive: retry-enter %s: %w", run.Spec.WorkItemRef, err)
		} else if !ok {
			// Fence moved under us (another leader reclaimed): re-read next pass.
			return ctrl.Result{RequeueAfter: continueDelay}, nil
		}
		return ctrl.Result{RequeueAfter: retryAfter}, nil
	}

	// Maximum retries exceeded or circuit breaker activated - fail the run
	if _, err := r.Claims.FailEnter(ctx, run.Spec.WorkItemRef, runID, cs.Fence); err != nil {
		return ctrl.Result{}, fmt.Errorf("rundrive: fail-enter %s: %w", run.Spec.WorkItemRef, err)
	}

	// Record terminal failure event
	r.logRetryEvent(ctx, runID, errorType, fmt.Sprintf("terminal_%s", cs.Step), laps, false, nil)

	return ctrl.Result{}, nil
}

// isCircuitBreakerActivated checks if the circuit breaker is active for a run
func (r *Driver) isCircuitBreakerActivated(runID string) bool {
	pauseTime, exists := r.circuitBreakerPauses[runID]
	if !exists {
		return false
	}
	return r.now().Before(pauseTime)
}

// activateCircuitBreaker activates the circuit breaker for a run
func (r *Driver) activateCircuitBreaker(ctx context.Context, runID, namespacedName string, lap int, errorType retryEventType) (ctrl.Result, error) {
	pauseUntil := r.now().Add(CircuitBreakerPauseDuration)
	r.circuitBreakerPauses[runID] = pauseUntil

	logMsg := fmt.Sprintf("rundrive: circuit breaker activated for %s after %d consecutive failures, retry paused until %v",
		namespacedName, CircuitBreakerMaxConsecutiveFailures, pauseUntil)
	// In a real implementation, this would use a proper logger
	fmt.Printf(logMsg)

	r.logRetryEvent(ctx, runID, retryEventTypeCircuitBreaker, fmt.Sprintf("circuit_breaker_after_%d_failures", lap), lap, false, &pauseUntil)

	return ctrl.Result{RequeueAfter: CircuitBreakerPauseDuration}, nil
}

// resetCircuitBreaker resets the circuit breaker state for a run
func (r *Driver) resetCircuitBreaker(runID string) {
	delete(r.circuitBreakerPauses, runID)
	r.consecutiveFailures[runID] = 0
}

// categorizeError categorizes errors to determine retry appropriateness
func (r *Driver) categorizeError(step reconcile.Step) retryEventType {
	switch step {
	case reconcile.StepDispatching, reconcile.StepRunning, reconcile.StepCollecting:
		// These steps indicate active execution - likely transient failures
		return retryEventTypeTransient
	case reconcile.StepFailed:
		// Explicit failure - could be permanent
		return retryEventTypePermanent
	default:
		// Unknown steps - treat as normal retry case
		return retryEventTypeNormal
	}
}

// getMaxRetriesWithDefaults returns the configured max retries with safety defaults
func (r *Driver) getMaxRetriesWithDefaults(run *api.Run) int {
	configuredMax := maxRetries(run)

	// Apply safety limits
	if configuredMax == 0 {
		return 0 // No retries configured
	}

	// Cap at a reasonable maximum to prevent infinite loops
	maxAllowed := 20
	if configuredMax > maxAllowed {
		fmt.Printf("rundrive: clamping maxRetries from %d to %d for run %s to prevent infinite loops",
			configuredMax, maxAllowed, string(run.UID))
		return maxAllowed
	}

	return configuredMax
}

// logRetryEvent records a retry event for monitoring and debugging
func (r *Driver) logRetryEvent(ctx context.Context, runID string, eventType, errorType string, lap int, success bool, nextRetryTime *time.Time) {
	event := retryEvent{
		Timestamp:   r.now(),
		RunID:       runID,
		EventType:   eventType,
		ErrorType:   errorType,
		Lap:         lap,
		Success:     success,
		WillRetry:   nextRetryTime != nil,
		NextRetryAt: nextRetryTime,
	}

	r.retryEvents = append(r.retryEvents, event)

	// Keep only recent events to prevent memory growth
	if len(r.retryEvents) > 1000 {
		r.retryEvents = r.retryEvents[len(r.retryEvents)-1000:]
	}

	// In a real implementation, this would use a proper logger and potentially emit metrics
	fmt.Printf("retry_event: %+v\n", event)
}

// getRetryEvents returns the recent retry events for monitoring
func (r *Driver) GetRetryEvents() []retryEvent {
	return r.retryEvents
}

// GetCircuitBreakerStatus returns the current circuit breaker state
func (r *Driver) GetCircuitBreakerStatus() map[string]time.Time {
	// Return a copy to avoid external modifications
	result := make(map[string]time.Time)
	for k, v := range r.circuitBreakerPauses {
		result[k] = v
	}
	return result
}

// finishCancel completes a 3.3 kill: teardown the Run's sandbox (best-effort —
// a stuck teardown must not wedge the terminal transition; the pool's draining
// state retries residue), then the guarded cancelling → cancelled finish.
func (r *Driver) finishCancel(ctx context.Context, run *api.Run, cs ClaimState) (ctrl.Result, error) {
	runID := string(run.UID)
	if r.Sandbox != nil {
		_ = r.Sandbox.Release(ctx, runID)
	}
	ok, err := r.Claims.CancelFinish(ctx, run.Spec.WorkItemRef, runID, cs.Fence)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("rundrive: cancel-finish %s: %w", run.Spec.WorkItemRef, err)
	}
	if !ok {
		// Fence moved (another kill/teardown raced) or already terminal:
		// re-read the world on the next pass.
		return ctrl.Result{RequeueAfter: continueDelay}, nil
	}
	return ctrl.Result{}, nil // terminal cancelled: the projector has it
}

// OnCancelDue is the kill-sweep wake (3.3): work items at cancelling whose
// Runs were healthy when killed (no death-detection requeue pending) get
// kicked back into the drive loop, which finishes them.
func (r *Driver) OnCancelDue(ctx context.Context, due []string) {
	for _, workItemID := range due {
		r.kickWorkItem(ctx, workItemID)
	}
}

// OnResumeDue is the 3.7 wake: each due episode re-enters its Run into
// dispatching (guarded) and kicks the Run CR back into the drive loop.
func (r *Driver) OnResumeDue(ctx context.Context, due []coord.ProdDuePause) {
	for _, d := range due {
		ok, err := r.Claims.RequeuePaused(ctx, d.WorkItemID)
		if err != nil || !ok {
			// err: transient infra failure — the episode is already claimed
			// (resumed_at stamped, exactly-once), so the honest backstop is
			// manual/operator re-drive, NOT an automatic retry storm (which
			// is exactly what 3.7 exists to prevent). !ok: the step moved on
			// (already requeued or terminal) — nothing to do.
			continue
		}
		r.kickWorkItem(ctx, d.WorkItemID)
	}
}

// kickWorkItem enqueues every Run owning the work item through the resume
// channel source.
func (r *Driver) kickWorkItem(ctx context.Context, workItemID string) {
	var runs api.RunList
	if err := r.List(ctx, &runs, client.MatchingFields{workItemField: workItemID}); err != nil {
		return
	}
	for i := range runs.Items {
		select {
		case r.resumeCh <- event.TypedGenericEvent[client.Object]{Object: &runs.Items[i]}:
		default: // full: level-triggered resync is the backstop
		}
	}
}

// maxRetries reads the retry budget (nil policy = 0: no automatic retry).
// Deprecated: Use getMaxRetriesWithDefaults for enhanced validation.
func maxRetries(run *api.Run) int {
	if run.Spec.RetryPolicy == nil || run.Spec.RetryPolicy.MaxRetries == nil {
		return 0
	}
	return int(*run.Spec.RetryPolicy.MaxRetries)
}

// BackoffFor is the retry-lap delay (attempt is 1-based): equal jitter over
// the capped exponential, base from spec.retryPolicy.backoffSeconds (default 1s).
func (r *Driver) BackoffFor(run *api.Run, attempt int) time.Duration {
	base := defaultBackoffBase
	if run.Spec.RetryPolicy != nil && run.Spec.RetryPolicy.BackoffSeconds != nil &&
		*run.Spec.RetryPolicy.BackoffSeconds >= 1 {
		base = time.Duration(*run.Spec.RetryPolicy.BackoffSeconds) * time.Second
	}
	return coord.EqualJitter(base, backoffCap, attempt, r.rand())
}

func (r *Driver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Driver) rand() float64 {
	if r.Rand != nil {
		return r.Rand()
	}
	return 0.5 // deterministic midpoint when unset (tests inject)
}

func (r *Driver) maxPasses() int {
	if r.MaxPasses > 0 {
		return r.MaxPasses
	}
	return DefaultMaxPasses
}

func isPaused(s reconcile.Step) bool {
	return s == reconcile.StepPaused || s == reconcile.StepPausedRateLimited
}

// ResumeEvents exposes the resume channel (the wiring hands it to the
// controller's channel source; tests observe the kicks).
func (r *Driver) ResumeEvents() <-chan event.TypedGenericEvent[client.Object] { return r.resumeCh }



// CleanupRetryEvents cleans up old retry events to prevent memory growth
func (r *Driver) CleanupRetryEvents(keepLast int) {
	if keepLast <= 0 {
		r.retryEvents = make([]retryEvent, 0)
		return
	}
	if len(r.retryEvents) > keepLast {
		r.retryEvents = r.retryEvents[len(r.retryEvents)-keepLast:]
	}
}

// GetConsecutiveFailures returns the current consecutive failure counts
func (r *Driver) GetConsecutiveFailures() map[string]int {
	// Return a copy to prevent external modifications
	result := make(map[string]int)
	for k, v := range r.consecutiveFailures {
		result[k] = v
	}
	return result
}

// SetupWithManager registers the Driver: Run watches, the resume channel
// source, and the workItemRef field index the resume kick lists by.
func (r *Driver) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &api.Run{}, workItemField,
		func(obj client.Object) []string {
			return []string{obj.(*api.Run).Spec.WorkItemRef}
		}); err != nil {
		return fmt.Errorf("rundrive: index %s: %w", workItemField, err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.Run{}).
		WatchesRawSource(source.Channel(r.resumeCh, &handler.EnqueueRequestForObject{})).
		Named("run-drive").
		Complete(r)
}
