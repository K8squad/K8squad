package apiserver

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// TestShortRunToken pins the run-scope token derivation to the same shape
// rundrive.ProgressMirror stamps into the `[run <id>]` comment prefix: the first
// 8 chars of the Run.UID (ISI-5130).
func TestShortRunToken(t *testing.T) {
	assert.Equal(t, "51c621b4", shortRunToken("51c621b4-aaaa-bbbb-cccc-dddddddddddd"))
	assert.Equal(t, "short", shortRunToken("short"))
	assert.Equal(t, "", shortRunToken(""))
}

// TestPopulateThinkingScopesToRun proves the ISI-5130 fix: the coord.comment read
// is scoped to THIS run — the query carries the `[run <token>]%` LIKE pattern so
// Postgres drops sibling-run rows, and only unscoped (ticket) + this-run rows
// come back and map into the timeline. Before the fix the query took a single
// arg (work_item_id) and every sibling run's tool/narration leaked in.
func TestPopulateThinkingScopesToRun(t *testing.T) {
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	run := &ksquadv1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name: "intake-ef5b2075-r83",
			UID:  types.UID("51c621b4-aaaa-bbbb-cccc-dddddddddddd"),
		},
		Spec: ksquadv1.RunSpec{WorkItemRef: "ef5b2075-1111-2222-3333-444455556666"},
	}
	svc := NewRunsServiceWithDB(nil, db)
	response := &RunDetailResponse{Run: run}

	// The scoped read must pass BOTH the work-item id and this run's `[run …]`
	// pattern. sqlmock does not evaluate LIKE, so we return exactly the rows a
	// scoped query yields (an unscoped ticket comment + this run's tool row) and
	// assert they land in the timeline.
	mock.ExpectQuery("FROM coord.comment").
		WithArgs("ef5b2075-1111-2222-3333-444455556666", "[run 51c621b4]%").
		WillReturnRows(sqlmock.NewRows([]string{"author_principal", "body", "created_at"}).
			AddRow("user:admin", "please look at r83", metav1.Now().Time).
			AddRow("run/51c621b4", "[run 51c621b4][tool:bash/result(ok)] done", metav1.Now().Time))

	assert.NoError(t, svc.populateThinking(ctx, response))
	// ExpectationsWereMet fails if the query ran without the run-scope arg — that
	// is the core regression guard for ISI-5130.
	assert.NoError(t, mock.ExpectationsWereMet())
	assert.Len(t, response.Thinking, 2)
}
