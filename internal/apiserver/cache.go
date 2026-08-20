package apiserver

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// ============================================================================
// Informer-cache backing for the squad-overview read model (ISI-2760).
// ============================================================================
//
// overview.go is deliberately cluster-free: it projects from any client.Reader, so it is unit
// testable against a fake client. This file is the ONE place that touches the cluster — it stands
// up a controller-runtime cache (shared informers over Team/Project/Run), waits for the initial
// sync, and hands its client.Reader to the read model. Keeping the wiring here means a cluster-less
// dev/test run never imports rest.Config machinery through the projection path.

// NewCacheOverviewReader builds the squad-overview read model over a live controller-runtime cache
// (the informer store §17.3 reads from). It resolves the rest.Config the standard way
// (in-cluster ServiceAccount, then KUBECONFIG / --kubeconfig), registers api/v1alpha1 on a fresh
// scheme, starts the cache, and blocks until the initial informer sync completes or syncTimeout
// elapses. The returned stop function tears the cache's goroutine down on shutdown.
//
// It fails (returns an error) rather than degrading silently when the cluster is unreachable: the
// caller (cmd/apiserver) decides whether that is fatal or a fall-back to the documented 501.
func NewCacheOverviewReader(ctx context.Context, syncTimeout time.Duration) (reader *ClientOverviewReader, stop func(), err error) {
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

	syncCtx, syncCancel := context.WithTimeout(cacheCtx, syncTimeout)
	defer syncCancel()
	if !c.WaitForCacheSync(syncCtx) {
		cancel()
		return nil, nil, fmt.Errorf("informer cache did not sync within %s", syncTimeout)
	}

	return NewClientOverviewReader(c), cancel, nil
}

// NewCacheCredentialReader builds the 8.6 credential/auth-state read model (ISI-2902) over a live
// controller-runtime cache — same discipline as NewCacheOverviewReader, informers over
// Team/Agent/Run. Fails (rather than degrading silently) when the cluster is unreachable; the
// caller decides fatal-vs-501.
func NewCacheCredentialReader(ctx context.Context, syncTimeout time.Duration) (reader *ClientCredentialReader, stop func(), err error) {
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

	cacheCtx, cancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- c.Start(cacheCtx) }()

	syncCtx, syncCancel := context.WithTimeout(cacheCtx, syncTimeout)
	defer syncCancel()
	if !c.WaitForCacheSync(syncCtx) {
		cancel()
		return nil, nil, fmt.Errorf("informer cache did not sync within %s", syncTimeout)
	}

	return NewClientCredentialReader(c), cancel, nil
}
