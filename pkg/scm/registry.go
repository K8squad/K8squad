/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scm

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// ProviderFactory builds a connected provider for one Project from its BYO
// credential (story 11.1 AC5). Factories are registered per provider name at
// composition time; the RECONCILE LOOP never switches on a provider name —
// it asks the registry and talks to whatever SourceControlProvider comes
// back (AC1, §10.2).
type ProviderFactory func(ctx context.Context, creds ProviderCredentials) (SourceControlProvider, error)

// ProviderRegistry maps provider names to factories. It is the ONLY place a
// concrete provider name is branched on — the composition root, not the
// control loop.
type ProviderRegistry struct {
	mu        sync.RWMutex
	factories map[string]ProviderFactory
}

// NewProviderRegistry returns a registry with the v1 GitHub provider
// pre-registered. GitHub Enterprise Server deployments register an
// additional factory with the enterprise baseURL at wiring time.
func NewProviderRegistry() *ProviderRegistry {
	r := &ProviderRegistry{factories: map[string]ProviderFactory{}}
	r.Register("github", func(ctx context.Context, creds ProviderCredentials) (SourceControlProvider, error) {
		return NewGitHubProvider("", creds)
	})
	return r
}

// Register adds (or replaces) the factory for a provider name. Registering
// a drop-in provider (GitLab, Gitea) is how §5.4/§10.2 provider extension
// happens — with zero reconciler change.
func (r *ProviderRegistry) Register(name string, factory ProviderFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = factory
}

// Names returns the registered provider names, sorted.
func (r *ProviderRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.factories))
	for name := range r.factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Provider builds the named provider. An unknown name is an error the
// reconciler surfaces on the Project status — never a silent skip.
func (r *ProviderRegistry) Provider(ctx context.Context, name string, creds ProviderCredentials) (SourceControlProvider, error) {
	r.mu.RLock()
	factory, ok := r.factories[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no source-control provider registered for %q (registered: %v)", name, r.Names())
	}
	return factory(ctx, creds)
}
