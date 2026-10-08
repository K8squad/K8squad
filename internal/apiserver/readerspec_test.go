package apiserver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	teamctrl "github.com/K8squad/K8squad/pkg/controller/team"
	"github.com/K8squad/K8squad/pkg/workspace"
)

// readerspecFixture pins the CRD + coord rows every case shares: one Team (metadata namespace
// "squad", sandbox namespace "squad-sandbox") owning one Project "demo" (UID projUID).
const (
	rsTeamUID   = "11111111-1111-1111-1111-111111111111"
	rsProjUID   = "22222222-2222-2222-2222-222222222222"
	rsRunID     = "33333333-3333-3333-3333-333333333333"
	rsCommitSHA = "0123456789abcdef0123456789abcdef01234567"
	rsGhostUID  = "99999999-9999-9999-9999-999999999999"
)

// readerspecReader builds the fake cluster both resolver readers ride. One client carries BOTH
// schemes (ksquad CRDs for the reader field, corev1 for the pod-scan field) and is handed in for
// both arguments — production wires two different clients (cache + direct), but both projections
// are Reader-interface pure, so a single fake is faithful.
//
// When teamStatusNS is non-empty the fixture also includes the project workspace PVC in that
// namespace (ISI-5574): ResolveReaderSpec now checks PVC existence before launching a reader pod,
// so every test that reaches that gate needs the claim to be present.
func readerspecReader(t *testing.T, teamStatusNS string) *fake.ClientBuilder {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ksquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	team := &ksquadv1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "squad", Namespace: "squad", UID: rsTeamUID},
	}
	team.Status.Namespace = teamStatusNS
	proj := &ksquadv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "squad", UID: rsProjUID},
	}
	objs := []client.Object{team, proj}
	if teamStatusNS != "" {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      workspace.ProjectPVCName("demo"),
				Namespace: teamStatusNS,
				Labels: map[string]string{
					workspace.LabelWorkspace: "true",
					workspace.LabelProject:   "demo",
				},
				Annotations: map[string]string{
					"k8squad.io/created-by": "project-pvc-controller",
				},
			},
		}
		objs = append(objs, pvc)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...)
}

func readerspecAuth() discussion.AuthorContext {
	return discussion.AuthorContext{Principal: "viewer@example.com", TeamID: uuid.MustParse(rsTeamUID)}
}

// claimTeamRows mocks the Phase-1 busy query: one row per claiming team, no rows = no live claim.
func claimTeamRows(teamIDs ...string) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"team_id"})
	for _, id := range teamIDs {
		rows.AddRow(id)
	}
	return rows
}

// holderPod builds a pod that mounts the Project PVC. readOnly mirrors how the volume is mounted
// (agent sandbox = RW, reader pod = RO); phase drives the terminal/non-terminal branch.
func holderPod(name, pvc string, readOnly bool, phase corev1.PodPhase) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "squad-sandbox"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: "workspace",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvc,
					ReadOnly:  readOnly,
				},
			},
		}}},
	}
	pod.Status.Phase = phase
	return pod
}

// zombieRuns builds n Run CRs for project "demo" stuck in Running/Claiming phase with no backing
// pod — the exact k8squad-test bmad-demo-project shape (ISI-5437: 24 Running + 10 Claiming).
func zombieRuns(n int) []client.Object {
	runs := make([]client.Object, 0, n)
	for i := 0; i < n; i++ {
		run := &ksquadv1.Run{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("zombie-%d", i), Namespace: "squad-sandbox"},
			Spec:       ksquadv1.RunSpec{ProjectRef: ksquadv1.ObjectRef{Name: "demo"}},
		}
		if i%2 == 0 {
			run.Status.Phase = ksquadv1.RunPhaseRunning
		} else {
			run.Status.Phase = ksquadv1.RunPhaseClaiming
		}
		runs = append(runs, run)
	}
	return runs
}

// errPodReader is a corev1-capable reader whose List always errors — drives the fail-safe branch.
type errPodReader struct{ err error }

func (e errPodReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return e.err
}

func (e errPodReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return e.err
}

func TestCoordReaderSpecResolver_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// No live claim (no claiming teams) ⇒ PVC free.
	mock.ExpectQuery("FROM coord.claim").
		WithArgs(rsProjUID).
		WillReturnRows(claimTeamRows())
	// Pin the browse-target SEMANTICS, not just the table (ISI-4693 review F3): a matcher on the
	// table alone survives mutating event_type/to_state/ORDER BY. These fragments assert the query
	// selects the project's LATEST SUCCEEDED terminal run, deterministically.
	mock.ExpectQuery(regexp.QuoteMeta("al.event_type = 'run_terminal'")).
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}).AddRow(rsRunID, rsCommitSHA, rsTeamUID))

	fakeReader := readerspecReader(t, "squad-sandbox").Build()
	r, err := NewCoordReaderSpecResolver(db, fakeReader, fakeReader)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	spec, err := r.ResolveReaderSpec(discussion.WithAuth(context.Background(), readerspecAuth()), "demo")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if spec.RunID != rsRunID {
		t.Errorf("RunID = %q, want %q", spec.RunID, rsRunID)
	}
	if spec.ProjectPVCName != workspace.ProjectPVCName("demo") {
		t.Errorf("ProjectPVCName = %q, want %q", spec.ProjectPVCName, workspace.ProjectPVCName("demo"))
	}
	if spec.CommitSHA != rsCommitSHA {
		t.Errorf("CommitSHA = %q, want %q", spec.CommitSHA, rsCommitSHA)
	}
	if spec.ReaderSAName != teamctrl.AgentServiceAccount {
		t.Errorf("ReaderSAName = %q, want %q", spec.ReaderSAName, teamctrl.AgentServiceAccount)
	}
	if spec.Namespace != "squad-sandbox" {
		t.Errorf("Namespace = %q, want the team sandbox namespace %q", spec.Namespace, "squad-sandbox")
	}
	if err := spec.Validate(); err != nil {
		t.Errorf("spec must satisfy readerpod.Spec.Validate: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql: %v", err)
	}
}

// Busy requires a LIVE pod physically mounting the RWO PVC read-write in the claiming team's
// sandbox namespace (ISI-5437). A live RW holder ⇒ ErrWorkspaceBusy, no browse-target query.
func TestCoordReaderSpecResolver_Busy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM coord.claim").
		WithArgs(rsProjUID).
		WillReturnRows(claimTeamRows(rsTeamUID))

	fakeReader := readerspecReader(t, "squad-sandbox").
		WithObjects(holderPod("sandbox-live", workspace.ProjectPVCName("demo"), false, corev1.PodRunning)).
		Build()
	r, err := NewCoordReaderSpecResolver(db, fakeReader, fakeReader)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = r.ResolveReaderSpec(discussion.WithAuth(context.Background(), readerspecAuth()), "demo")
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("err = %v, want ErrWorkspaceBusy", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql: %v", err)
	}
}

// TestCoordReaderSpecResolver_BusyZombieRunsFreePVC is the ISI-5437 regression guard: the exact
// k8squad-test shape — 34 zombie Run CRs (Running/Claiming, no backing pod) behind live coord
// claims, and the PVC physically free. The busy decision must key on the PHYSICAL pod scan, not
// Run CR phase, so the reader spec resolves instead of wedging on workspace_busy forever.
func TestCoordReaderSpecResolver_BusyZombieRunsFreePVC(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// Live claims (zombie runs) held by the team — Phase 1 reports the team.
	mock.ExpectQuery("FROM coord.claim").
		WithArgs(rsProjUID).
		WillReturnRows(claimTeamRows(rsTeamUID))
	// PVC is physically free ⇒ not busy ⇒ the browse target resolves.
	mock.ExpectQuery("FROM coord.audit_log").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}).AddRow(rsRunID, rsCommitSHA, rsTeamUID))

	fakeReader := readerspecReader(t, "squad-sandbox").
		WithObjects(zombieRuns(34)...). // 17 Running + 17 Claiming zombies, zero pods
		Build()
	r, err := NewCoordReaderSpecResolver(db, fakeReader, fakeReader)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	spec, err := r.ResolveReaderSpec(discussion.WithAuth(context.Background(), readerspecAuth()), "demo")
	if err != nil {
		t.Fatalf("zombie runs + free PVC must resolve, got err: %v", err)
	}
	if spec.RunID != rsRunID {
		t.Errorf("RunID = %q, want %q", spec.RunID, rsRunID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql: %v", err)
	}
}

// A live reader pod's READ-ONLY mount must not read as busy: the busy gate launches those reader
// pods itself, so counting RO mounts would permanently self-inflict workspace_busy.
func TestCoordReaderSpecResolver_BusyReaderPodROMountDoesNotHold(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM coord.claim").
		WithArgs(rsProjUID).
		WillReturnRows(claimTeamRows(rsTeamUID))
	mock.ExpectQuery("FROM coord.audit_log").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}).AddRow(rsRunID, rsCommitSHA, rsTeamUID))

	fakeReader := readerspecReader(t, "squad-sandbox").
		WithObjects(append(zombieRuns(1),
			holderPod("reader-pod", workspace.ProjectPVCName("demo"), true, corev1.PodRunning), // RO co-mount
		)...).
		Build()
	r, err := NewCoordReaderSpecResolver(db, fakeReader, fakeReader)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if _, err := r.ResolveReaderSpec(discussion.WithAuth(context.Background(), readerspecAuth()), "demo"); err != nil {
		t.Fatalf("live RO reader mount must not read busy, got err: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql: %v", err)
	}
}

// A pod that reached a terminal phase has released its mounts — even with the volume still in
// its spec it must not read busy.
func TestCoordReaderSpecResolver_BusyTerminalPodDoesNotHold(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM coord.claim").
		WithArgs(rsProjUID).
		WillReturnRows(claimTeamRows(rsTeamUID))
	mock.ExpectQuery("FROM coord.audit_log").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}).AddRow(rsRunID, rsCommitSHA, rsTeamUID))

	fakeReader := readerspecReader(t, "squad-sandbox").
		WithObjects(holderPod("sandbox-done", workspace.ProjectPVCName("demo"), false, corev1.PodSucceeded)).
		Build()
	r, err := NewCoordReaderSpecResolver(db, fakeReader, fakeReader)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if _, err := r.ResolveReaderSpec(discussion.WithAuth(context.Background(), readerspecAuth()), "demo"); err != nil {
		t.Fatalf("terminal pod holding a stale volume spec must not read busy, got err: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql: %v", err)
	}
}

// Fail-safe: if the pod scan is unreachable, busy is conserved (true) — never launch a reader
// that could wedge Pending on an RWO Multi-Attach.
func TestCoordReaderSpecResolver_BusyPodListErrorFailsSafe(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM coord.claim").
		WithArgs(rsProjUID).
		WillReturnRows(claimTeamRows(rsTeamUID))

	fakeReader := readerspecReader(t, "squad-sandbox").Build()
	r, err := NewCoordReaderSpecResolver(db, fakeReader, errPodReader{err: errors.New("pod informer down")})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = r.ResolveReaderSpec(discussion.WithAuth(context.Background(), readerspecAuth()), "demo")
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("err = %v, want ErrWorkspaceBusy (fail-safe)", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql: %v", err)
	}
}

// Fail-safe: a live claim whose team cannot be resolved to a sandbox namespace cannot be
// physically disproven — busy is conserved.
func TestCoordReaderSpecResolver_BusyUnresolvableTeamFailsSafe(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM coord.claim").
		WithArgs(rsProjUID).
		WillReturnRows(claimTeamRows(rsGhostUID)) // no Team CR with this UID

	fakeReader := readerspecReader(t, "squad-sandbox").Build()
	r, err := NewCoordReaderSpecResolver(db, fakeReader, fakeReader)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = r.ResolveReaderSpec(discussion.WithAuth(context.Background(), readerspecAuth()), "demo")
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("err = %v, want ErrWorkspaceBusy (fail-safe)", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql: %v", err)
	}
}

// TestCoordReaderSpecResolver_BusyStaleClaimTerminalRun verifies ADR-0025 D3: a live coord.claim
// row with an unexpired lease must NOT block the explorer when nothing physically holds the PVC
// (here: the corresponding Run CR already reached a terminal phase and no pod mounts the claim).
func TestCoordReaderSpecResolver_BusyStaleClaimTerminalRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// DB reports a live claim (stale — run completed but claim not yet cleared).
	mock.ExpectQuery("FROM coord.claim").
		WithArgs(rsProjUID).
		WillReturnRows(claimTeamRows(rsTeamUID))
	// Run CR is Succeeded and no pod mounts the PVC → physically free. The browse target exists.
	mock.ExpectQuery("FROM coord.audit_log").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}).AddRow(rsRunID, rsCommitSHA, rsTeamUID))

	run := &ksquadv1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-done", Namespace: "squad-sandbox"},
		Spec:       ksquadv1.RunSpec{ProjectRef: ksquadv1.ObjectRef{Name: "demo"}},
	}
	run.Status.Phase = ksquadv1.RunPhaseSucceeded
	fakeReader := readerspecReader(t, "squad-sandbox").WithObjects(run).Build()
	r, err := NewCoordReaderSpecResolver(db, fakeReader, fakeReader)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	spec, err := r.ResolveReaderSpec(discussion.WithAuth(context.Background(), readerspecAuth()), "demo")
	if err != nil {
		t.Fatalf("stale-claim + terminal run should resolve, got err: %v", err)
	}
	if spec.RunID != rsRunID {
		t.Errorf("RunID = %q, want %q", spec.RunID, rsRunID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql: %v", err)
	}
}

func TestCoordReaderSpecResolver_NoBrowseTarget(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM coord.claim").
		WithArgs(rsProjUID).
		WillReturnRows(claimTeamRows())
	// No completed (succeeded) Run yet ⇒ nothing to browse.
	mock.ExpectQuery("FROM coord.audit_log").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}))

	fakeReader := readerspecReader(t, "squad-sandbox").Build()
	r, err := NewCoordReaderSpecResolver(db, fakeReader, fakeReader)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = r.ResolveReaderSpec(discussion.WithAuth(context.Background(), readerspecAuth()), "demo")
	if !errors.Is(err, ErrNoBrowseTarget) {
		t.Fatalf("err = %v, want ErrNoBrowseTarget", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql: %v", err)
	}
}

// ISI-4693: a completed Run with NO build-snapshot (the common case — the runtime writes plain
// files, not git commits) still resolves a valid browse target. The reader serves the live Project
// workspace RO, so an absent commit is a valid target, not ErrNoBrowseTarget, and the resulting Spec
// must still validate (CommitSHA is no longer required).
func TestCoordReaderSpecResolver_CompletedRunNoSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM coord.claim").
		WithArgs(rsProjUID).
		WillReturnRows(claimTeamRows())
	// Completed Run, LEFT JOIN yields NULL commit (no build-snapshot artifact captured).
	mock.ExpectQuery("FROM coord.audit_log").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}).AddRow(rsRunID, nil, rsTeamUID))

	fakeReader := readerspecReader(t, "squad-sandbox").Build()
	r, err := NewCoordReaderSpecResolver(db, fakeReader, fakeReader)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	spec, err := r.ResolveReaderSpec(discussion.WithAuth(context.Background(), readerspecAuth()), "demo")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if spec.RunID != rsRunID {
		t.Errorf("RunID = %q, want %q", spec.RunID, rsRunID)
	}
	if spec.CommitSHA != "" {
		t.Errorf("CommitSHA = %q, want empty (no snapshot captured)", spec.CommitSHA)
	}
	if spec.ProjectPVCName != workspace.ProjectPVCName("demo") {
		t.Errorf("ProjectPVCName = %q, want %q", spec.ProjectPVCName, workspace.ProjectPVCName("demo"))
	}
	if spec.Namespace != "squad-sandbox" {
		t.Errorf("Namespace = %q, want %q", spec.Namespace, "squad-sandbox")
	}
	if err := spec.Validate(); err != nil {
		t.Errorf("a completed-Run spec with no commit must still validate: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql: %v", err)
	}
}

// ISI-4693 review F3/F6: pin the browse-target query's correctness guards so a regression that
// loosens them fails HERE, not silently in production. Each sub-case matches a distinct required
// SQL fragment; sqlmock fails the query if the fragment is absent, so dropping any one guard breaks
// the test.
func TestCoordReaderSpecResolver_QueryGuardsPinned(t *testing.T) {
	for _, frag := range []string{
		"al.to_state = 'succeeded'",                                      // only SUCCEEDED terminals
		"ORDER BY al.created_at DESC, al.id DESC",                        // deterministic latest (F6 tiebreaker)
		"w.team_id IS NOT NULL",                                          // F2: skip team-less items, never 500
		"a.work_item_id = al.work_item_id AND a.kind = 'build-snapshot'", // F6: correlated, no fan-out
	} {
		t.Run(frag, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			mock.ExpectQuery("FROM coord.claim").
				WithArgs(rsProjUID).
				WillReturnRows(claimTeamRows())
			mock.ExpectQuery(regexp.QuoteMeta(frag)).
				WithArgs(rsProjUID).
				WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}).AddRow(rsRunID, rsCommitSHA, rsTeamUID))

			fakeReader := readerspecReader(t, "squad-sandbox").Build()
			r, err := NewCoordReaderSpecResolver(db, fakeReader, fakeReader)
			if err != nil {
				t.Fatalf("construct: %v", err)
			}
			if _, err := r.ResolveReaderSpec(discussion.WithAuth(context.Background(), readerspecAuth()), "demo"); err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("query missing required guard %q: %v", frag, err)
			}
		})
	}
}

// TestCoordReaderSpecResolver_WorkspaceNotProvisioned verifies ISI-5574: when the project's
// workspace PVC does not exist in the team's sandbox namespace, ResolveReaderSpec returns
// ErrWorkspaceNotProvisioned rather than launching a reader pod that would wedge Pending forever.
func TestCoordReaderSpecResolver_WorkspaceNotProvisioned(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM coord.claim").
		WithArgs(rsProjUID).
		WillReturnRows(claimTeamRows())
	mock.ExpectQuery("FROM coord.audit_log").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}).AddRow(rsRunID, rsCommitSHA, rsTeamUID))

	// Manually build a reader with team+project and a real sandbox namespace but NO workspace PVC.
	scheme := runtime.NewScheme()
	if err := ksquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	team := &ksquadv1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "squad", Namespace: "squad", UID: rsTeamUID},
	}
	team.Status.Namespace = "squad-sandbox"
	proj := &ksquadv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "squad", UID: rsProjUID},
	}
	// No PVC object — the whole point of this test.
	noPVCReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team, proj).Build()
	r, err := NewCoordReaderSpecResolver(db, noPVCReader, noPVCReader)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = r.ResolveReaderSpec(discussion.WithAuth(context.Background(), readerspecAuth()), "demo")
	if !errors.Is(err, ErrWorkspaceNotProvisioned) {
		t.Fatalf("err = %v, want ErrWorkspaceNotProvisioned", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql: %v", err)
	}
}

func TestCoordReaderSpecResolver_TeamNamespacePending(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM coord.claim").
		WithArgs(rsProjUID).
		WillReturnRows(claimTeamRows())
	mock.ExpectQuery("FROM coord.audit_log").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}).AddRow(rsRunID, rsCommitSHA, rsTeamUID))

	// Team reconciler has not stamped status.namespace yet ⇒ the claim cannot exist anywhere.
	fakeReader := readerspecReader(t, "").Build()
	r, err := NewCoordReaderSpecResolver(db, fakeReader, fakeReader)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = r.ResolveReaderSpec(discussion.WithAuth(context.Background(), readerspecAuth()), "demo")
	if !errors.Is(err, ErrNoBrowseTarget) {
		t.Fatalf("err = %v, want ErrNoBrowseTarget", err)
	}
}

func TestCoordReaderSpecResolver_FailClosed(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	fakeReader := readerspecReader(t, "squad-sandbox").Build()
	if _, err := NewCoordReaderSpecResolver(nil, fakeReader, fakeReader); err == nil {
		t.Error("nil db must fail closed")
	}
	if _, err := NewCoordReaderSpecResolver(db, nil, fakeReader); err == nil {
		t.Error("nil reader must fail closed")
	}
	if _, err := NewCoordReaderSpecResolver(db, fakeReader, nil); err == nil {
		t.Error("nil pod reader must fail closed")
	}

	r, err := NewCoordReaderSpecResolver(db, fakeReader, fakeReader)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	// No author on the context ⇒ fail closed, never resolve.
	if _, err := r.ResolveReaderSpec(context.Background(), "demo"); err == nil {
		t.Error("missing author context must fail closed")
	}
	// Unknown project ⇒ existence-hiding 404.
	if _, err := r.ResolveReaderSpec(discussion.WithAuth(context.Background(), readerspecAuth()), "ghost"); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("unknown project err = %v, want ErrProjectNotFound", err)
	}
	// Empty project id ⇒ 404, not a cluster call.
	if _, err := r.ResolveReaderSpec(discussion.WithAuth(context.Background(), readerspecAuth()), ""); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("empty project err = %v, want ErrProjectNotFound", err)
	}
}

// The busy predicate must treat an expired lease as reclaimable, not held: the SQL itself filters
// lease_expires_at > now(). Pin the fragment so a regression that drops the lease window fails here.
func TestCoordReaderSpecResolver_BusyQueryHonoursLeaseExpiry(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("c.lease_expires_at IS NULL OR c.lease_expires_at > now()")).
		WithArgs(rsProjUID).
		WillReturnRows(claimTeamRows()) // expired lease ⇒ no claiming team ⇒ free
	mock.ExpectQuery("FROM coord.audit_log").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}))

	fakeReader := readerspecReader(t, "squad-sandbox").Build()
	r, err := NewCoordReaderSpecResolver(db, fakeReader, fakeReader)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if _, err := r.ResolveReaderSpec(discussion.WithAuth(context.Background(), readerspecAuth()), "demo"); !errors.Is(err, ErrNoBrowseTarget) {
		t.Fatalf("err = %v, want ErrNoBrowseTarget", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql: %v", err)
	}
}

// TestCoordReaderSpecResolver_Generation pins the ADR-0025 D5a coherence contract for the
// generation token (ISI-5484 acceptance). generation MUST:
//
//	(a) be stable across repeated resolves of an unchanged idle project,
//	(b) change on a busy↔idle flip,
//	(c) change when the succeeded browse-target Run UID changes, AND
//	(d) return to the IDENTICAL idle token after a FAILED/CANCELLED run flips busy→idle without
//	    advancing the succeeded browse-target (review C2) — which documents that coherence across
//	    that edge rests on the client's mandatory revalidate-on-hit, NOT on generation, and guards
//	    against a regressive "force-invalidate on every busy→idle" that would break (a).
func TestCoordReaderSpecResolver_Generation(t *testing.T) {
	ctx := discussion.WithAuth(context.Background(), readerspecAuth())
	const (
		runA = "33333333-3333-3333-3333-333333333333"
		runB = "44444444-4444-4444-4444-444444444444"
	)

	// idleGen resolves generation for an idle project whose latest succeeded browse-target is runID.
	idleGen := func(t *testing.T, runID string) string {
		t.Helper()
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock: %v", err)
		}
		defer db.Close()
		// No claiming teams ⇒ not busy ⇒ the browse-target query runs.
		mock.ExpectQuery("FROM coord.claim").
			WithArgs(rsProjUID).
			WillReturnRows(claimTeamRows())
		mock.ExpectQuery(regexp.QuoteMeta("al.event_type = 'run_terminal'")).
			WithArgs(rsProjUID).
			WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}).AddRow(runID, rsCommitSHA, rsTeamUID))
		fakeReader := readerspecReader(t, "squad-sandbox").Build()
		r, err := NewCoordReaderSpecResolver(db, fakeReader, fakeReader)
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		gen, err := r.ResolveGeneration(ctx, "demo")
		if err != nil {
			t.Fatalf("resolve generation (idle %s): %v", runID, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sql: %v", err)
		}
		return gen
	}

	// busyGen resolves generation while a live pod physically holds the RWO PVC (busy epoch). The
	// browse-target query MUST NOT run — the busy token is returned at the busy gate.
	busyGen := func(t *testing.T) string {
		t.Helper()
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock: %v", err)
		}
		defer db.Close()
		mock.ExpectQuery("FROM coord.claim").
			WithArgs(rsProjUID).
			WillReturnRows(claimTeamRows(rsTeamUID))
		fakeReader := readerspecReader(t, "squad-sandbox").
			WithObjects(holderPod("sandbox-live", workspace.ProjectPVCName("demo"), false, corev1.PodRunning)).
			Build()
		r, err := NewCoordReaderSpecResolver(db, fakeReader, fakeReader)
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		gen, err := r.ResolveGeneration(ctx, "demo")
		if err != nil {
			t.Fatalf("resolve generation (busy): %v", err)
		}
		// ExpectationsWereMet also asserts the browse-target query was NOT issued on the busy path.
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sql: %v", err)
		}
		return gen
	}

	// (a) stable across repeated idle resolves of an unchanged project.
	first := idleGen(t, runA)
	if first == "" {
		t.Fatal("idle generation must be non-empty")
	}
	if again := idleGen(t, runA); again != first {
		t.Errorf("generation not stable across identical idle resolves: %q vs %q", first, again)
	}

	// (b) changes on an idle→busy flip.
	busy := busyGen(t)
	if busy == "" {
		t.Fatal("busy generation must be non-empty")
	}
	if busy == first {
		t.Errorf("generation must change on an idle→busy flip, got identical token %q", busy)
	}

	// (c) changes when the succeeded browse-target Run UID changes.
	if genB := idleGen(t, runB); genB == first {
		t.Errorf("generation must change when the browse-target Run UID changes (%s→%s), got identical token %q", runA, runB, genB)
	}

	// (d) C2: a failed run flips busy→idle without advancing the succeeded browse-target, so the
	// idle token returns to its ORIGINAL value — the same project, same latest succeeded run.
	if afterFailed := idleGen(t, runA); afterFailed != first {
		t.Errorf("after a failed-run busy→idle cycle (same succeeded target), generation must return to the original idle token: got %q, want %q", afterFailed, first)
	}
}

// TestGenToken_OpaqueAndDistinct pins the token primitive directly: deterministic for identical
// inputs, and distinct across the three coherence-relevant states (busy, idle-empty, idle-with-target).
func TestGenToken_OpaqueAndDistinct(t *testing.T) {
	idleA := genToken("run-a", false)
	idleB := genToken("run-b", false)
	idleEmpty := genToken("", false)
	busy := genToken("", true)

	if idleA != genToken("run-a", false) {
		t.Error("genToken must be deterministic for identical inputs")
	}
	distinct := map[string]string{"idleA": idleA, "idleB": idleB, "idleEmpty": idleEmpty, "busy": busy}
	seen := map[string]string{}
	for name, tok := range distinct {
		if tok == "" {
			t.Errorf("%s token is empty", name)
		}
		if other, dup := seen[tok]; dup {
			t.Errorf("token collision: %s and %s both hash to %q", name, other, tok)
		}
		seen[tok] = name
	}
}
