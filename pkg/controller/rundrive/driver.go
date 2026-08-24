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
	"log/slog"
	"math/rand"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/reconcile"
	"github.com/K8squad/K8squad/pkg/telemetry"
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
)

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
		Client:    cl,
		Claims:    claims,
		Pauses:    pauses,
		Runner:    runner,
		MaxPasses: DefaultMaxPasses,
		Rand:      rand.Float64,
		resumeCh:  make(chan event.TypedGenericEvent[client.Object], 64),
	}
}

// Reconcile is one level-triggered drive pass over the Run's durable state.
//
// Every drivable Run pass opens exactly one span (ISI-2915 AC3): the operator's
// OTel spine gives each Run a distributed trace. Inbound W3C trace-context on
// the Run's annotations makes this span a child of whoever enqueued the Run;
// absent that, it roots a fresh trace. The span context flows into the
// Claims/Runner/reconcile calls below, and a correlated slog line ties the log
// stream to the trace (AC4). The named err return lets one deferred hook record
// failure on the span across every return path.
func (r *Driver) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, err error) {
	var run api.Run
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if run.DeletionTimestamp != nil || run.Spec.WorkItemRef == "" {
		return ctrl.Result{}, nil
	}
	runID := string(run.UID)

	ctx = telemetry.Extract(ctx, run.Annotations)
	ctx, span := telemetry.Tracer().Start(ctx, "run.reconcile", trace.WithAttributes(
		attribute.String("ksquad.run.id", runID),
		attribute.String("ksquad.run.work_item_ref", run.Spec.WorkItemRef),
		attribute.String("ksquad.run.namespace", run.Namespace),
		attribute.String("ksquad.run.name", run.Name),
	))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()
	slog.InfoContext(ctx, "rundrive: driving run",
		"run.id", runID, "run.work_item_ref", run.Spec.WorkItemRef)

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
func (r *Driver) retryOrFail(ctx context.Context, run *api.Run, cs ClaimState) (ctrl.Result, error) {
	runID := string(run.UID)
	if r.Sandbox != nil {
		// Best-effort: a stuck teardown must not block the custody fix; the
		// pool's draining state retries it (§9.3).
		_ = r.Sandbox.Release(ctx, runID)
	}
	laps, err := r.Claims.LapsUsed(ctx, runID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("rundrive: count laps for %s: %w", runID, err)
	}
	if laps < maxRetries(run) {
		if _, ok, err := r.Claims.RetryEnter(ctx, run.Spec.WorkItemRef, runID, cs.Fence); err != nil {
			return ctrl.Result{}, fmt.Errorf("rundrive: retry-enter %s: %w", run.Spec.WorkItemRef, err)
		} else if !ok {
			// Fence moved under us (another leader reclaimed): re-read next pass.
			return ctrl.Result{RequeueAfter: continueDelay}, nil
		}
		return ctrl.Result{RequeueAfter: r.BackoffFor(run, laps+1)}, nil
	}
	if _, err := r.Claims.FailEnter(ctx, run.Spec.WorkItemRef, runID, cs.Fence); err != nil {
		return ctrl.Result{}, fmt.Errorf("rundrive: fail-enter %s: %w", run.Spec.WorkItemRef, err)
	}
	return ctrl.Result{}, nil
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
