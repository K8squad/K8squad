package memory

import (
	"context"
	"fmt"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// teamresolver.go — the memory service's binding of coord.TeamAgentResolver
// (ADR-0024 §8 / ISI-4743). It supplies the agent-∈-Team authority the
// PM→implementer assign verb (work_item_assign → coord.AgentRequestDispatch)
// needs so cmd/memory can drive a real dispatch, not just refuse it honestly.
//
// Team composition (Team.Spec.Agents) lives ONLY in the Team CR (Kubernetes) —
// coord's schema keys work_item.team_id on the Team CR uid but stores no member
// list, so a DB-backed resolver is not possible here (ISI-4743 scope note). We
// therefore mirror the apiserver's resolver (internal/apiserver/teamagentresolve.go):
// resolve a Team's members through a client.Reader backed by ONE shared informer
// cache over Team CRs. The cache is an in-memory list, so TeamAgents is a local
// read — safe to call inside coord's dispatch transaction, exactly as the
// apiserver's is (the same "in-memory list, no network I/O inside the txn"
// invariant coord.RequestDispatch relies on).
//
// Fail-open, matching the rest of cmd/memory (discussion_post, the authoring
// store): if the cluster is unreachable or the SA lacks the Team read grant, the
// caller leaves the dispatch backend nil and work_item_assign stays honestly
// unavailable (ErrAssignUnavailable) — create + update still serve, and the
// reads never go down. It never silently degrades to authorizing against an empty
// world.

// NewTeamCacheReader builds the ONE shared informer cache over ksquad Team CRs and
// returns its client.Reader for the assign resolver. It resolves the rest.Config
// the standard way (in-cluster ServiceAccount, then KUBECONFIG / --kubeconfig),
// starts the cache, and blocks until the initial Team informer sync completes or
// syncTimeout elapses. The returned stop function tears the cache goroutine down
// on shutdown.
//
// Like the apiserver's NewCacheReader it FAILS (returns an error) rather than
// degrading silently when the cluster is unreachable: the caller (cmd/memory)
// treats that as "no dispatch backend" and keeps work_item_assign honestly
// unavailable, never as authorization against an empty Team world.
func NewTeamCacheReader(ctx context.Context, syncTimeout time.Duration) (reader client.Reader, stop func(), err error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve kube config: %w", err)
	}

	scheme := runtime.NewScheme()
	if err := ksquadv1.AddToScheme(scheme); err != nil {
		return nil, nil, fmt.Errorf("register ksquad scheme: %w", err)
	}

	c, err := cache.New(cfg, cache.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, fmt.Errorf("build informer cache: %w", err)
	}

	// Run the cache until its context is cancelled; stop() cancels it.
	cacheCtx, cancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- c.Start(cacheCtx) }()

	// Wait for sync while racing a cache-start failure (e.g. unreachable API server)
	// so the error surfaces immediately instead of masquerading as a sync timeout.
	syncCtx, syncCancel := context.WithTimeout(cacheCtx, syncTimeout)
	defer syncCancel()
	synced := make(chan bool, 1)
	go func() { synced <- c.WaitForCacheSync(syncCtx) }()
	select {
	case err := <-errCh:
		cancel()
		return nil, nil, fmt.Errorf("informer cache failed to start: %w", err)
	case ok := <-synced:
		if !ok {
			cancel()
			return nil, nil, fmt.Errorf("informer cache did not sync within %s", syncTimeout)
		}
	}

	return c, cancel, nil
}

// ClientTeamAgentResolver resolves Team.Spec.Agents through a client.Reader (the
// shared informer cache) — an in-memory list, so it is safe to call inside coord's
// dispatch transaction. It mirrors the apiserver's clientTeamAgentResolver so the
// memory service authorizes an assign against the EXACT Team composition Intake
// dispatches from.
type ClientTeamAgentResolver struct {
	reader client.Reader
}

// NewClientTeamAgentResolver binds the resolver to a Team-reading client.Reader.
func NewClientTeamAgentResolver(reader client.Reader) *ClientTeamAgentResolver {
	return &ClientTeamAgentResolver{reader: reader}
}

// TeamAgents lists the agent names of the Team whose CR uid is teamUID
// (coord.work_item.team_id), implementing coord.TeamAgentResolver. A uid that
// resolves to no Team CR is an ERROR, not a silent empty set: dispatch must fail
// loudly on a dangling team rather than vacuously reject every agent as "not a
// member" — the same contract the apiserver's resolver keeps.
func (r *ClientTeamAgentResolver) TeamAgents(ctx context.Context, teamUID string) ([]string, error) {
	if teamUID == "" {
		return nil, fmt.Errorf("memory.TeamAgents: empty team uid")
	}
	var teams ksquadv1.TeamList
	if err := r.reader.List(ctx, &teams); err != nil {
		return nil, fmt.Errorf("memory.TeamAgents: list teams: %w", err)
	}
	for i := range teams.Items {
		t := &teams.Items[i]
		if string(t.UID) == teamUID {
			names := make([]string, 0, len(t.Spec.Agents))
			for _, ref := range t.Spec.Agents {
				names = append(names, ref.Name)
			}
			sort.Strings(names)
			return names, nil
		}
	}
	return nil, fmt.Errorf("memory.TeamAgents: team uid %s resolves to no Team CR", teamUID)
}
