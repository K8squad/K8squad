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
	mock.ExpectQuery("FROM coord.artifact").
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
	// No build-snapshot artifact yet ⇒ nothing to browse.
	mock.ExpectQuery("FROM coord.artifact").
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

func TestCoordReaderSpecResolver_TeamNamespacePending(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM coord.claim").
		WithArgs(rsProjUID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("FROM coord.artifact").
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
	mock.ExpectQuery("FROM coord.artifact").
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
