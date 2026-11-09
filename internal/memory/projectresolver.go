package memory

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// projectresolver.go — ISI-7228 GAP-1: the memory service's Project-ref normalizer for the
// discussionindex seam. The recall side narrows by Project CR uid (pkg/contextasm gathers with
// string(req.Project.UID); coord.work_item.project_id stores the uid), but legacy
// discussion.thread rows carry the "namespace/name" route slug (ISI-3982 domain drift) — a
// slug-stamped memory record is invisible to project-narrowed recall in BOTH run contexts. This
// resolver maps the slug to the Project CR uid through ONE shared informer cache over Project
// CRs, exactly as teamresolver.go mirrors Team composition for the assign resolver: an in-memory
// list, safe to call from the best-effort indexer sweep, fail-open on every error (the seam then
// stamps the verbatim ref — the record never becomes LESS recallable than before).
type ProjectRefResolver struct {
	reader client.Reader
	stop   context.CancelFunc

	mu    sync.Mutex
	byRef map[string]string // "namespace/name" slug → Project CR uid
}

// NewProjectRefResolver builds the ONE shared informer cache over ksquad Project CRs and returns
// the slug→uid resolver. It resolves the rest.Config the standard way (in-cluster ServiceAccount,
// then KUBECONFIG / --kubeconfig), starts the cache, and blocks until the initial Project informer
// sync completes or syncTimeout elapses. The cache runs until ctx is cancelled — tie its lifetime
// to the service context, exactly like the indexer goroutine it serves.
func NewProjectRefResolver(ctx context.Context, syncTimeout time.Duration) (*ProjectRefResolver, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("resolve kube config: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := ksquadv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register ksquad scheme: %w", err)
	}
	c, err := cache.New(cfg, cache.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("build informer cache: %w", err)
	}
	cacheCtx, cancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- c.Start(cacheCtx) }()
	syncCtx, syncCancel := context.WithTimeout(cacheCtx, syncTimeout)
	defer syncCancel()
	synced := make(chan bool, 1)
	go func() { synced <- c.WaitForCacheSync(syncCtx) }()
	select {
	case err := <-errCh:
		cancel()
		return nil, fmt.Errorf("informer cache failed to start: %w", err)
	case ok := <-synced:
		if !ok {
			cancel()
			return nil, fmt.Errorf("informer cache did not sync within %s", syncTimeout)
		}
	}
	return &ProjectRefResolver{reader: c, stop: cancel, byRef: map[string]string{}}, nil
}

// ResolveProjectUID implements discussionindex.ProjectResolver: it maps a thread's
// "namespace/name" project slug to the Project CR uid the assembler narrows recall by.
// Resolution is memoized; a miss re-lists the (local, in-memory) informer state once and
// re-checks. Unknown refs return ok=false — the seam then stamps the verbatim value (fail-open).
func (r *ProjectRefResolver) ResolveProjectUID(ctx context.Context, projectRef string) (string, bool) {
	if projectRef == "" {
		return "", false
	}
	r.mu.Lock()
	uid, ok := r.byRef[projectRef]
	r.mu.Unlock()
	if ok {
		return uid, true
	}
	var projects ksquadv1.ProjectList
	if err := r.reader.List(ctx, &projects); err != nil {
		return "", false
	}
	r.mu.Lock()
	for i := range projects.Items {
		p := &projects.Items[i]
		r.byRef[p.Namespace+"/"+p.Name] = string(p.UID)
	}
	uid, ok = r.byRef[projectRef]
	r.mu.Unlock()
	return uid, ok
}
