package apiserver

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// ============================================================================
// Informer-cache backing for the cache-backed read models (ISI-2760 8.1 overview,
// ISI-2902 8.6 credentials).
// ============================================================================
//
// overview.go and credentials.go are deliberately cluster-free: they project from any
// client.Reader, so they are unit-testable against a fake client. This file is the ONE place
// that touches the cluster — it stands up a single controller-runtime cache (shared informers
// over Team/Project/Agent/Run) shared by BOTH read models, waits for the initial sync, and hands
// its client.Reader to the projections. One cache, not one-per-reader: a second cache would
// duplicate every Team/Run watch connection and deep-copy every object twice in memory (Run
// status with conditions is the expensive kind to double). Keeping the wiring here means a
// cluster-less dev/test run never imports rest.Config machinery through the projection path.

// NewCacheReader builds the ONE shared informer cache over the ksquad CRDs and returns its
// client.Reader for the cache-backed read models (8.1 overview, 8.6 credentials). It resolves
// the rest.Config the standard way (in-cluster ServiceAccount, then KUBECONFIG / --kubeconfig),
// starts the cache, and blocks until the initial informer sync completes or syncTimeout elapses.
// The returned stop function tears the cache's goroutine down on shutdown.
//
// It fails (returns an error) rather than degrading silently when the cluster is unreachable: the
// caller (cmd/apiserver) decides whether that is fatal or a fall-back to the documented 501.
func NewCacheReader(ctx context.Context, syncTimeout time.Duration) (reader client.Reader, stop func(), err error) {
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

	// Wait for sync while racing a cache-start failure (e.g. unreachable API server) so the
	// error surfaces immediately instead of masquerading as a sync timeout.
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
