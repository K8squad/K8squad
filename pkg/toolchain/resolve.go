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

package toolchain

import (
	"context"
	"fmt"
	"sort"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// RefSeparator splits the name@version ref grammar
// (Skill.spec.requires.toolchains, arch §5.3.4).
const RefSeparator = "@"

// ParseRef splits a "name@version" toolchain ref. Exactly one "@" with
// non-empty sides is valid; anything else is an actionable error, never a
// silent default (fail-closed).
func ParseRef(ref string) (name, version string, err error) {
	name, version, found := strings.Cut(ref, RefSeparator)
	if !found {
		return "", "", fmt.Errorf("toolchain ref %q is missing a version; use the name@version form, e.g. %s@1.31", ref, ref)
	}
	if strings.Contains(version, RefSeparator) {
		return "", "", fmt.Errorf("toolchain ref %q has more than one %q; use the name@version form", ref, RefSeparator)
	}
	if strings.TrimSpace(name) == "" || strings.TrimSpace(version) == "" {
		return "", "", fmt.Errorf("toolchain ref %q has an empty name or version; use the name@version form", ref)
	}
	return name, version, nil
}

// UnknownError names a ref (or bare name) that resolves nowhere — no
// team-namespace Toolchain, no cluster-catalog entry.
type UnknownError struct {
	Ref     string
	Looked  []string
	Details string
}

func (e *UnknownError) Error() string {
	return fmt.Sprintf("toolchain %q has no Toolchain in %s%s", e.Ref, strings.Join(e.Looked, " or "), suffixDetails(e.Details))
}

// VersionError names a ref whose Toolchain exists but does not carry the
// requested version pin.
type VersionError struct {
	Ref      string
	Availab  []string
	Details  string
	Override bool
}

func (e *VersionError) Error() string {
	where := "cluster catalog"
	if e.Override {
		where = "the team-namespace override (narrowed from the cluster catalog)"
	}
	return fmt.Sprintf("toolchain %q: version not offered by %s; available: %s%s",
		e.Ref, where, strings.Join(e.Availab, ", "), suffixDetails(e.Details))
}

// ConflictError names two skills pinning the same toolchain name at
// different versions — the §5.3.4 fail-closed union conflict.
type ConflictError struct {
	Name    string
	Have    string
	Want    string
	Details string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("toolchain %q version conflict: %s vs %s — a Run's skills must pin one version per toolchain%s",
		e.Name, e.Have, e.Want, suffixDetails(e.Details))
}

// TrustError names a narrow-only / cluster-scope trust-boundary violation
// observed at resolution time (admission normally catches these first;
// the resolver re-asserts them fail-closed so drift — a bypassed webhook,
// a direct-etcd edit — can never silently widen).
type TrustError struct {
	Override string
	Reason   string
}

func (e *TrustError) Error() string {
	return fmt.Sprintf("team-namespace Toolchain %s violates the narrow-only override boundary: %s", e.Override, e.Reason)
}

func suffixDetails(details string) string {
	if details == "" {
		return ""
	}
	return " (" + details + ")"
}

// Resolved is one toolchain pinned for a Run: the catalog entry that won,
// the exact image to stage, and the effective (possibly narrowed) RBAC
// envelope the renderer unions.
type Resolved struct {
	// Name is the catalog name ("kubectl").
	Name string `json:"name"`
	// Version is the pinned version string ("1.31").
	Version string `json:"version"`
	// Image is the resolved, recorded-in-status OCI reference.
	Image string `json:"image"`
	// Provides is the winning entry's declared binary surface.
	Provides []string `json:"provides,omitempty"`
	// SourceNamespace is the namespace the winning version entry came
	// from (the override when one applies, else the cluster catalog) —
	// provenance for the capability manifest.
	SourceNamespace string `json:"sourceNamespace"`
	// RBAC is the effective envelope after override narrowing; nil when
	// the toolchain declares no RBAC (staging only, no rules granted).
	RBAC *api.ToolchainRBAC `json:"rbac,omitempty"`
}

// Resolver resolves toolchain refs against the catalog through a
// controller-runtime reader. The same instance serves admission (webhook)
// and assembly (renderer) so both walk identical semantics.
type Resolver struct {
	Reader   client.Reader
	Platform PlatformConfig
}

func (r *Resolver) platform() PlatformConfig {
	return r.Platform.WithDefaults()
}

// ResolveRefs resolves a Run's full name@version ref set, fail-closed:
// unknown names, unknown versions, version conflicts and narrow-only
// boundary violations all error. The result is sorted by name so callers
// (status recording, rendering) see a stable order.
func (r *Resolver) ResolveRefs(ctx context.Context, teamNamespace string, refs []string, details string) ([]Resolved, error) {
	// Per-name pins, preserving first-seen order for conflict messages.
	type pin struct {
		version string
		ref     string
	}
	pins := map[string]pin{}
	var order []string
	for _, ref := range refs {
		name, version, err := ParseRef(ref)
		if err != nil {
			return nil, err
		}
		if first, seen := pins[name]; seen {
			if first.version != version {
				return nil, &ConflictError{Name: name, Have: first.version, Want: version, Details: details}
			}
			continue
		}
		pins[name] = pin{version: version, ref: ref}
		order = append(order, name)
	}

	var resolved []Resolved
	for _, name := range order {
		pin := pins[name]
		res, err := r.resolveOne(ctx, teamNamespace, name, pin.version, details)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, *res)
	}
	sort.Slice(resolved, func(i, j int) bool { return resolved[i].Name < resolved[j].Name })
	return resolved, nil
}

// resolveOne resolves one name@version pin. Resolution order: the
// team-namespace override (narrow-only, admission-guaranteed) wins when
// present, else the cluster catalog entry.
func (r *Resolver) resolveOne(ctx context.Context, teamNamespace, name, version, details string) (*Resolved, error) {
	platform := r.platform()

	var override, catalog *api.Toolchain
	if teamNamespace != "" && !r.platform().IsClusterCatalog(teamNamespace) {
		override = &api.Toolchain{}
		if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: teamNamespace, Name: name}, override); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("read team Toolchain %s/%s (fail-closed): %w", teamNamespace, name, err)
			}
			override = nil
		}
	}
	catalog = &api.Toolchain{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: platform.ClusterCatalogNamespace, Name: name}, catalog); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("read catalog Toolchain %s/%s (fail-closed): %w", platform.ClusterCatalogNamespace, name, err)
		}
		catalog = nil
	}

	if override == nil && catalog == nil {
		return nil, &UnknownError{Ref: name + RefSeparator + version,
			Looked:  []string{teamNamespace, platform.ClusterCatalogNamespace},
			Details: details}
	}

	// Trust boundary (plan §2.2b): a team-namespace Toolchain is an
	// override of the cluster-catalog authority — it must have a
	// counterpart, and may only narrow. The webhook enforces this at
	// admission; resolution re-asserts it fail-closed against drift.
	if override != nil {
		if catalog == nil {
			return nil, &TrustError{Override: teamNamespace + "/" + name,
				Reason: fmt.Sprintf("no cluster-catalog Toolchain %s/%s to override; define the catalog entry first (team namespaces cannot mint toolchains)", platform.ClusterCatalogNamespace, name)}
		}
		if err := ValidateNarrowing(override, catalog, platform, details); err != nil {
			return nil, err
		}
	}

	source := pickSource(override, catalog)
	entry, found := findVersion(source, version)
	if !found {
		avail := versionStrings(source)
		return nil, &VersionError{Ref: name + RefSeparator + version, Availab: avail,
			Details: details, Override: override != nil}
	}

	return &Resolved{
		Name:            name,
		Version:         entry.Version,
		Image:           entry.Image,
		Provides:        entry.Provides,
		SourceNamespace: source.Namespace,
		RBAC:            effectiveRBAC(override, catalog, platform),
	}, nil
}

// pickSource returns the entry list that governs version availability:
// the override when present (its versions are admission-constrained to a
// subset of the catalog's), else the catalog.
func pickSource(override, catalog *api.Toolchain) *api.Toolchain {
	if override != nil {
		return override
	}
	return catalog
}

func findVersion(tc *api.Toolchain, version string) (api.ToolchainVersion, bool) {
	for _, v := range tc.Spec.Versions {
		if v.Version == version {
			return v, true
		}
	}
	return api.ToolchainVersion{}, false
}

func versionStrings(tc *api.Toolchain) []string {
	if tc == nil {
		return nil
	}
	out := make([]string, 0, len(tc.Spec.Versions))
	for _, v := range tc.Spec.Versions {
		out = append(out, v.Version)
	}
	return out
}

// effectiveRBAC computes the RBAC envelope a Run's renderer unions: the
// override's narrowed rules when it declares any, else the catalog's. A
// nil override RBAC narrows nothing. The override's scope may equal the
// catalog's or narrow cluster → namespace; namespace → cluster widening
// is rejected by validateNarrowing.
func effectiveRBAC(override, catalog *api.Toolchain, platform PlatformConfig) *api.ToolchainRBAC {
	base := catalogRBAC(catalog)
	if override == nil || override.Spec.RBAC == nil {
		return base
	}
	narrowed := override.Spec.RBAC.DeepCopy()
	// If the override omitted scope, it inherits the catalog's intent —
	// which validateNarrowing already proved is not a widening.
	if narrowed.Scope == "" && base != nil {
		narrowed.Scope = base.Scope
	}
	if narrowed.Scope == "" {
		narrowed.Scope = api.ToolchainRBACScopeNamespace
	}
	return narrowed
}

func catalogRBAC(catalog *api.Toolchain) *api.ToolchainRBAC {
	if catalog == nil || catalog.Spec.RBAC == nil {
		return nil
	}
	return catalog.Spec.RBAC
}

// validateNarrowing re-asserts the §2.2b trust boundary between a team
// override and the cluster-catalog authority: every version pin must
// exist in the catalog with the identical image (no minted images), every
// RBAC rule must be a subset of a catalog rule (no widened or new rules),
// and scope may only stay or narrow. Admission guarantees all of this;
// drift fails closed here.
func ValidateNarrowing(override, catalog *api.Toolchain, platform PlatformConfig, details string) error {
	catalogVersions := map[string]string{}
	for _, v := range catalog.Spec.Versions {
		catalogVersions[v.Version] = v.Image
	}
	for _, v := range override.Spec.Versions {
		want, ok := catalogVersions[v.Version]
		if !ok {
			return &TrustError{Override: override.Namespace + "/" + override.Name,
				Reason: fmt.Sprintf("offers version %q that the cluster catalog does not; overrides may only narrow the offered version set%s", v.Version, suffixDetails(details))}
		}
		if want != v.Image {
			return &TrustError{Override: override.Namespace + "/" + override.Name,
				Reason: fmt.Sprintf("pins version %q to image %q but the cluster catalog pins %q; image substitution is not narrowing%s", v.Version, v.Image, want, suffixDetails(details))}
		}
	}

	if oRBAC := override.Spec.RBAC; oRBAC != nil {
		cRBAC := catalog.Spec.RBAC
		if cRBAC == nil || len(cRBAC.Rules) == 0 {
			return &TrustError{Override: override.Namespace + "/" + override.Name,
				Reason: fmt.Sprintf("declares rbac but the cluster-catalog Toolchain grants none; RBAC is honored only from the cluster catalog%s", suffixDetails(details))}
		}
		if oRBAC.Scope == api.ToolchainRBACScopeCluster {
			return &TrustError{Override: override.Namespace + "/" + override.Name,
				Reason: fmt.Sprintf("sets rbac.scope=cluster; cluster scope is never renderable from a team namespace%s", suffixDetails(details))}
		}
		for _, rule := range oRBAC.Rules {
			if !RuleCoveredBy(rule, cRBAC.Rules) {
				return &TrustError{Override: override.Namespace + "/" + override.Name,
					Reason: fmt.Sprintf("grants rule %+v that no cluster-catalog rule covers; overrides may only narrow rules to a subset%s", rule, suffixDetails(details))}
			}
		}
	}
	return nil
}

// RuleCoveredBy reports whether candidate is a subset of some rule in
// base: every list dimension (verbs, apiGroups, resources,
// resourceNames, nonResourceURLs) no wider than the covering rule. Empty
// string lists mean "all", so an empty candidate dimension is only
// covered when the base dimension is empty too.
func RuleCoveredBy(candidate rbacv1.PolicyRule, base []rbacv1.PolicyRule) bool {
	for _, b := range base {
		if subsetStrings(candidate.Verbs, b.Verbs) &&
			subsetStrings(candidate.APIGroups, b.APIGroups) &&
			subsetStrings(candidate.Resources, b.Resources) &&
			subsetStrings(candidate.ResourceNames, b.ResourceNames) &&
			subsetStrings(candidate.NonResourceURLs, b.NonResourceURLs) {
			return true
		}
	}
	return false
}

// subsetStrings implements the "empty = universe" subset: a dimension
// that says "everything" is only covered by another "everything".
func subsetStrings(sub, sup []string) bool {
	if len(sub) == 0 {
		return len(sup) == 0
	}
	set := make(map[string]bool, len(sup))
	for _, s := range sup {
		set[s] = true
	}
	for _, s := range sub {
		if !set[s] {
			return false
		}
	}
	return true
}

// RuleKey is the canonical identity of a PolicyRule for union dedupe —
// order inside a rule's lists is not semantic, so the key sorts them.
func RuleKey(r rbacv1.PolicyRule) string {
	part := func(xs []string) string {
		if len(xs) == 0 {
			return "*"
		}
		sorted := append([]string(nil), xs...)
		sort.Strings(sorted)
		return strings.Join(sorted, ",")
	}
	return strings.Join([]string{
		part(r.APIGroups), part(r.Resources), part(r.Verbs),
		part(r.ResourceNames), part(r.NonResourceURLs),
	}, "|")
}

// UnionRules unions rule sets additively and dedupes by RuleKey (plan
// §2.2b: additive across toolchains, duplicates dedupe, never silently
// merged away — the recorded union IS the grant). First-seen order is
// kept so status bytes are stable across idempotent reconciles.
func UnionRules(sets ...[]rbacv1.PolicyRule) []rbacv1.PolicyRule {
	seen := map[string]bool{}
	var out []rbacv1.PolicyRule
	for _, set := range sets {
		for _, r := range set {
			k := RuleKey(r)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, r)
		}
	}
	return out
}
