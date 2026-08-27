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

package webhook

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/toolchain"
)

// SetupToolchainWebhookWithManager registers the Toolchain validating
// webhook (Epic B / ISI-3286, plan §2.2 + §2.2b): CRD shape discipline
// CEL cannot finish (wildcards, rule shape) plus the cross-object trust
// boundary — team-namespace Toolchains are narrow-only overrides of the
// cluster catalog, and cluster-scope RBAC needs the explicit platform
// opt-in. Deploys failurePolicy=fail.
func SetupToolchainWebhookWithManager(mgr ctrl.Manager) error {
	v := &ToolchainCustomValidator{Validator: &CrossRefValidator{Reader: mgr.GetClient()}}
	return ctrl.NewWebhookManagedBy(mgr, &ksquadv1alpha1.Toolchain{}).
		WithValidator(v).
		Complete()
}

// ToolchainCustomValidator validates Toolchain admission (story B1).
type ToolchainCustomValidator struct{ Validator *CrossRefValidator }

var _ admission.Validator[*ksquadv1alpha1.Toolchain] = &ToolchainCustomValidator{}

// ValidateCreate implements admission.Validator.
func (v *ToolchainCustomValidator) ValidateCreate(ctx context.Context, tc *ksquadv1alpha1.Toolchain) (admission.Warnings, error) {
	if tc == nil {
		return nil, apierrors.NewBadRequest("expected a Toolchain but got nil")
	}
	return nil, toInvalid("Toolchain", tc.Name, v.Validator.ValidateToolchain(ctx, tc))
}

// ValidateUpdate implements admission.Validator.
func (v *ToolchainCustomValidator) ValidateUpdate(ctx context.Context, _, newObj *ksquadv1alpha1.Toolchain) (admission.Warnings, error) {
	return v.ValidateCreate(ctx, newObj)
}

// ValidateDelete implements admission.Validator (Toolchains delete
// freely; the per-Run Role GC story owns cleanup).
func (v *ToolchainCustomValidator) ValidateDelete(_ context.Context, _ *ksquadv1alpha1.Toolchain) (admission.Warnings, error) {
	return nil, nil
}

// ValidateToolchain enforces the Epic B admission contract:
//
//   - shape: unique versions, images present, well-formed least-privilege
//     rules (no wildcards, every rule grants something);
//   - trust: team-namespace Toolchains may only NARROW a same-name
//     cluster-catalog entry (subset versions, identical images, subset
//     rules, no cluster scope); cluster scope on a catalog entry needs the
//     platform opt-in.
//
// Narrowing the CLUSTER entry later can strand pre-existing overrides in
// "wider than catalog" state; that drift fails closed at Run resolution
// (pkg/toolchain.ValidateNarrowing re-asserts), which is the accepted
// posture — the catalog is the authority.
func (v *CrossRefValidator) ValidateToolchain(ctx context.Context, tc *ksquadv1alpha1.Toolchain) field.ErrorList {
	var errs field.ErrorList
	if !v.on(GuardToolchainCatalog) {
		return errs
	}
	platform := v.Toolchains.WithDefaults()

	// Shape: strict version uniqueness (CEL only catches conflicting
	// duplicates; exact duplicates are still rejected here).
	seenVersions := map[string]string{}
	for i, ver := range tc.Spec.Versions {
		path := fmt.Sprintf("spec.versions[%d]", i)
		if prior, dup := seenVersions[ver.Version]; dup {
			errs = append(errs, invalidf(path+"/version", ver.Version,
				"duplicate version %q (first pinned at index %s); a version pins exactly one image", ver.Version, prior))
			continue
		}
		seenVersions[ver.Version] = fmt.Sprintf("%d", i)
		if ver.Image == "" {
			errs = append(errs, invalidf(path+"/image", ver.Image, "every version entry must pin an image (digest pins recommended)"))
		}
	}

	// Shape + least privilege: the catalog is curated (same D2 discipline
	// as the Team baseline Role — no wildcards, no hollow rules).
	if rb := tc.Spec.RBAC; rb != nil {
		for i, rule := range rb.Rules {
			path := fmt.Sprintf("spec.rbac.rules[%d]", i)
			for _, dim := range []struct {
				name string
				vals []string
			}{
				{"verbs", rule.Verbs},
				{"apiGroups", rule.APIGroups},
				{"resources", rule.Resources},
				{"nonResourceURLs", rule.NonResourceURLs},
			} {
				for _, val := range dim.vals {
					if val == "*" {
						errs = append(errs, invalidf(path+"."+dim.name, val,
							"wildcards are rejected: toolchain RBAC is curated least-privilege; enumerate the %s explicitly", dim.name))
					}
				}
			}
			if len(rule.Verbs) == 0 {
				errs = append(errs, invalidf(path+".verbs", rule.Verbs, "every rule must declare at least one verb"))
			}
			if len(rule.Resources) == 0 && len(rule.NonResourceURLs) == 0 {
				errs = append(errs, invalidf(path, rule, "every rule must target resources or nonResourceURLs; a hollow rule grants nothing"))
			}
			if len(rule.Resources) > 0 && len(rule.APIGroups) == 0 {
				errs = append(errs, invalidf(path+".apiGroups", rule.APIGroups,
					"apiGroups must be set when resources are (use [\"\"] for the core group)"))
			}
		}
		if rb.Scope == ksquadv1alpha1.ToolchainRBACScopeCluster && !platform.AllowClusterScope {
			errs = append(errs, invalidf("spec.rbac.scope", rb.Scope,
				"cluster-scope RBAC requires the explicit platform opt-in (Helm value tools.rbac.clusterScopeEnabled → %s); without it only namespace scope is admissible", toolchain.EnvAllowClusterScope))
		}
	}

	// Trust boundary: cluster catalog is the authority; team namespaces
	// only narrow it.
	if !platform.IsClusterCatalog(tc.Namespace) {
		catalog := &ksquadv1alpha1.Toolchain{}
		catalogNS := platform.ClusterCatalogNamespace
		ok, err := refExists(ctx, v.Reader, catalog, catalogNS, tc.Name)
		switch {
		case err != nil:
			errs = append(errs, invalidf("metadata.namespace", tc.Namespace,
				"admission read failed (fail-closed): %v; retry or check apiserver health", err))
		case !ok:
			errs = append(errs, invalidf("metadata.namespace", tc.Namespace,
				"team-namespace Toolchains may only override an existing cluster-catalog entry; define %s/%s first (the catalog is the platform-curated authority)", catalogNS, tc.Name))
		default:
			if trustErr := toolchain.ValidateNarrowing(tc, catalog, platform, ""); trustErr != nil {
				errs = append(errs, invalidf("spec", tc.Spec,
					"%v; overrides may only narrow — subset versions with identical images, subset RBAC rules, namespace scope", trustErr))
			}
		}
	}
	return errs
}
