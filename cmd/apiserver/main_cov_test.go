package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/apiserver"
)

// hasCoordinatorRole (ISI-5283 / ISI-5267 WS-2): the mention composer routes a
// 2+-mention post to the Team Coordinator, so the badge scan must be exact —
// any Coordinator badge is true, no badge (or no roles at all) is false.
func TestHasCoordinatorRole(t *testing.T) {
	coordinator := apiserver.OrgAgent{Roles: []apiserver.RoleBadge{
		{ID: "r1", Name: "ic"},
		{ID: "r2", Name: "coordinator", Coordinator: true},
	}}
	if !hasCoordinatorRole(coordinator) {
		t.Fatal("agent carrying a Coordinator badge must report true")
	}
	ic := apiserver.OrgAgent{Roles: []apiserver.RoleBadge{{ID: "r1", Name: "ic"}}}
	if hasCoordinatorRole(ic) {
		t.Fatal("agent without a Coordinator badge must report false")
	}
	if hasCoordinatorRole(apiserver.OrgAgent{}) {
		t.Fatal("roleless agent must report false")
	}
}

// errOrgReader fails exactly the three org reads the mentionRoster adapter
// wraps; the adapter must propagate them (empty roster + error, never a panic
// or a silent empty-success).
type errOrgReader struct{ stubOrgReader }

func (errOrgReader) Org(context.Context, string) (apiserver.TeamOrg, error) {
	return apiserver.TeamOrg{}, errors.New("org projection unavailable")
}

func (errOrgReader) NamespaceAgents(context.Context, string) ([]apiserver.OrgAgent, error) {
	return nil, errors.New("org projection unavailable")
}

func (errOrgReader) AllAgents(context.Context) ([]apiserver.OrgAgent, error) {
	return nil, errors.New("org projection unavailable")
}

func TestMentionRosterPropagatesOrgErrors(t *testing.T) {
	r := mentionRoster{org: errOrgReader{}}
	ctx := context.Background()

	if _, err := r.TeamAgents(ctx, uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")); err == nil {
		t.Fatal("TeamAgents must propagate the team org read error")
	}
	if _, err := r.ProjectAgents(ctx, "team-a/proj-x"); err == nil {
		t.Fatal("ProjectAgents must propagate the namespace roster read error")
	}
	if _, err := r.AllAgents(ctx); err == nil {
		t.Fatal("AllAgents must propagate the fleet roster read error")
	}
}

// failConnector yields a *sql.DB whose every dial fails — /readyz must be red
// (dbReady.Ready returns the ping error) rather than green-by-accident.
type failConnector struct{}

func (failConnector) Connect(context.Context) (driver.Conn, error) { return nil, errors.New("db down") }
func (failConnector) Driver() driver.Driver                        { return failDriver{} }

type failDriver struct{}

func (failDriver) Open(string) (driver.Conn, error) { return nil, errors.New("db down") }

func TestDBReadyPingFailsRed(t *testing.T) {
	db := sql.OpenDB(failConnector{})
	defer db.Close()
	if err := (dbReady{db: db}).Ready(context.Background()); err == nil {
		t.Fatal("/readyz must report red when the DB ping fails")
	}
}

// coordAuditWriter: a payload json.Marshal cannot encode (a channel) is
// logged loudly and dropped — never a panic, and the DB is never reached
// (nil *sql.DB here on purpose; the marshal failure returns first).
func TestCoordAuditWriterMarshalFailureIsLoudButSafe(t *testing.T) {
	w := coordAuditWriter(nil)
	w(context.Background(), "user.admin.create", "alice", map[string]any{"boom": make(chan int)})
}
