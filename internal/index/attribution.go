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

// Package index registers the cache field indexes backing RBAC scope
// queries (story 1.6 / ISI-2522, consumed by Epic 15.3 per-project RBAC).
package index

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// RegisterAttributionIndexes registers the attribution field indexes on the
// given indexer for each provided object type (story 1.6: Team, Project,
// Agent, Run — pass the concrete types once story 1.2's api package merges):
//
//	err := index.RegisterAttributionIndexes(ctx, mgr.GetCache(),
//		&ksquadv1.Team{}, &ksquadv1.Project{}, &ksquadv1.Agent{}, &ksquadv1.Run{})
//
// Registered keys (query with client.MatchingFields):
//
//   - ksquadv1.CreatedByIndexKey — metadata.annotations[ksquad.io/created-by]
//   - ksquadv1.OwnedByIndexKey   — spec.ownedBy
//
// These indexes are what makes resource-scoped permission checks cheap:
// listing every resource a principal owns is a cache index hit instead of a
// full informer scan.
func RegisterAttributionIndexes(ctx context.Context, indexer client.FieldIndexer, objs ...client.Object) error {
	for _, obj := range objs {
		if err := indexer.IndexField(ctx, obj, ksquadv1.CreatedByIndexKey, ksquadv1.CreatedByIndexValue); err != nil {
			return fmt.Errorf("registering %s index for %T: %w", ksquadv1.CreatedByIndexKey, obj, err)
		}
		if err := indexer.IndexField(ctx, obj, ksquadv1.OwnedByIndexKey, ksquadv1.OwnedByIndexValue); err != nil {
			return fmt.Errorf("registering %s index for %T: %w", ksquadv1.OwnedByIndexKey, obj, err)
		}
	}
	return nil
}
