package apiserver

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
)

func readerspecReader(t *testing.T, teamStatusNS string) *fake.ClientBuilder {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ksquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	team := &ksquadv1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "squad", Namespace: "squad", UID: rsTeamUID},
	}
	team.Status.Namespace = teamStatusNS
	proj := &ksquadv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "squad", UID: rsProjUID},
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(team, proj)
}

func readerspecAuth() discussion.AuthorContext {
	return discussion.AuthorContext{Principal: "viewer@example.com", TeamID: uuid.MustParse(rsTeamUID)}
}

func TestCoordReaderSpecResolver_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM coord.claim").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	// Pin the browse-target SEMANTICS, not just the table (ISI-4693 review F3): a matcher on the
	// table alone survives mutating event_type/to_state/ORDER BY. These fragments assert the query
	// selects the project's LATEST SUCCEEDED terminal run, deterministically.
	mock.ExpectQuery(regexp.QuoteMeta("al.event_type = 'run_terminal'")).
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}).AddRow(rsRunID, rsCommitSHA, rsTeamUID))

	r, err := NewCoordReaderSpecResolver(db, readerspecReader(t, "squad-sandbox").Build())
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

func TestCoordReaderSpecResolver_Busy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// A live claim on any of the project's work items ⇒ AC7 busy, no artifact query follows.
	mock.ExpectQuery("FROM coord.claim").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	r, err := NewCoordReaderSpecResolver(db, readerspecReader(t, "squad-sandbox").Build())
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

func TestCoordReaderSpecResolver_NoBrowseTarget(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM coord.claim").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	// No completed (succeeded) Run yet ⇒ nothing to browse.
	mock.ExpectQuery("FROM coord.audit_log").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}))

	r, err := NewCoordReaderSpecResolver(db, readerspecReader(t, "squad-sandbox").Build())
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
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	// Completed Run, LEFT JOIN yields NULL commit (no build-snapshot artifact captured).
	mock.ExpectQuery("FROM coord.audit_log").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}).AddRow(rsRunID, nil, rsTeamUID))

	r, err := NewCoordReaderSpecResolver(db, readerspecReader(t, "squad-sandbox").Build())
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
				WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
			mock.ExpectQuery(regexp.QuoteMeta(frag)).
				WithArgs(rsProjUID).
				WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}).AddRow(rsRunID, rsCommitSHA, rsTeamUID))

			r, err := NewCoordReaderSpecResolver(db, readerspecReader(t, "squad-sandbox").Build())
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

func TestCoordReaderSpecResolver_TeamNamespacePending(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM coord.claim").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("FROM coord.audit_log").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}).AddRow(rsRunID, rsCommitSHA, rsTeamUID))

	// Team reconciler has not stamped status.namespace yet ⇒ the claim cannot exist anywhere.
	r, err := NewCoordReaderSpecResolver(db, readerspecReader(t, "").Build())
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

	if _, err := NewCoordReaderSpecResolver(nil, readerspecReader(t, "squad-sandbox").Build()); err == nil {
		t.Error("nil db must fail closed")
	}
	if _, err := NewCoordReaderSpecResolver(db, nil); err == nil {
		t.Error("nil reader must fail closed")
	}

	r, err := NewCoordReaderSpecResolver(db, readerspecReader(t, "squad-sandbox").Build())
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
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("FROM coord.audit_log").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "commit", "team_id"}))

	r, err := NewCoordReaderSpecResolver(db, readerspecReader(t, "squad-sandbox").Build())
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
