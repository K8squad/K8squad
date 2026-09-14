// workitemdispatch_unit_test.go — the NO-Postgres unit lane for the board
// dispatch store (ADR-0022 / ISI-4411): the constructor guards and the
// fail-closed input guards that return BEFORE any BeginTx. The database-backed
// properties (agent-∈-Team enforcement, backlog→todo CAS, tenancy 404, audit
// co-commit, re-dispatch 409) are exercised by the 0021 migration self-check and
// the apiserver handler tests; here we pin only the branches that never touch the
// DB, so every `go test ./...` re-proves them.
package coord

import (
	"context"
	"errors"
	"testing"
)

// stubAgents is a TeamAgentResolver that never has to be consulted for the
// pre-BeginTx guards (they return first). It records whether it WAS consulted so
// a guard that leaks past the input check trips the test.
type stubAgents struct {
	names  []string
	err    error
	called bool
}

func (s *stubAgents) TeamAgents(_ context.Context, _ string) ([]string, error) {
	s.called = true
	return s.names, s.err
}

func newOfflineDispatchStore(t *testing.T, agents TeamAgentResolver) *WorkItemDispatchStore {
	t.Helper()
	s, err := NewWorkItemDispatchStore(offlineDB(), agents)
	if err != nil {
		t.Fatalf("NewWorkItemDispatchStore: %v", err)
	}
	return s
}

func TestNewWorkItemDispatchStoreRejectsNilDeps(t *testing.T) {
	if _, err := NewWorkItemDispatchStore(nil, &stubAgents{}); err == nil {
		t.Fatal("nil db must be rejected")
	}
	if _, err := NewWorkItemDispatchStore(offlineDB(), nil); err == nil {
		t.Fatal("nil team-agent resolver must be rejected (it is the §D4 auth source)")
	}
}

// TestRequestDispatchRejectsBadInput — the required-field guards fail closed with
// ErrInvalidWorkItem and never reach the (offline) DB or consult the resolver.
func TestRequestDispatchRejectsBadInput(t *testing.T) {
	cases := map[string]RequestDispatchInput{
		"no work item": {AgentID: "coder", Principal: "user:alice"},
		"no agent":     {WorkItemID: "wi-1", Principal: "user:alice"},
		"no principal": {WorkItemID: "wi-1", AgentID: "coder"},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			agents := &stubAgents{}
			s := newOfflineDispatchStore(t, agents)
			_, err := s.RequestDispatch(context.Background(), in)
			if !errors.Is(err, ErrInvalidWorkItem) {
				t.Fatalf("want ErrInvalidWorkItem, got %v", err)
			}
			if agents.called {
				t.Fatal("resolver must not be consulted before the input guards pass")
			}
		})
	}
}
