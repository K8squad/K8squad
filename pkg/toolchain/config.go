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

// Package toolchain resolves Skill.spec.requires.toolchains refs
// ("kubectl@1.31") against the Toolchain catalog (plan §2.2/§2.2b): the
// cluster catalog (admin-authored, the authority) plus team-namespace
// overrides that may only narrow. It is the single resolver shared by Run
// admission (fail-closed: unknown name/version and version conflicts
// reject the Run) and Run assembly's RBAC renderer, so what admission
// proved is what dispatch assumes.
package toolchain

import (
	"os"
	"strings"
)

// PlatformConfig carries the two platform-side facts the toolchain trust
// boundary needs (plan §2.2b). Both come from the Helm chart — the cluster
// catalog namespace is the control-plane namespace, and cluster-scope RBAC
// rides an explicit opt-in — injected as environment variables on the
// webhook and operator deployments (tools.rbac.clusterScopeEnabled).
type PlatformConfig struct {
	// ClusterCatalogNamespace is the namespace the admin-authored catalog
	// lives in (the control-plane namespace). Toolchains here are the
	// authority; same-name Toolchains elsewhere are narrow-only overrides.
	ClusterCatalogNamespace string

	// AllowClusterScope gates rbac.scope=cluster: admitted and rendered
	// ONLY when true. Default false — cluster scope is an explicit
	// platform decision, never a default.
	AllowClusterScope bool
}

// Environment variables the Helm chart sets on the webhook and operator
// deployments. The KSQUAD_ prefix mirrors the binary's other env surface.
const (
	EnvClusterCatalogNamespace = "KSQUAD_CLUSTER_CATALOG_NAMESPACE"
	EnvAllowClusterScope       = "KSQUAD_TOOLCHAIN_CLUSTER_SCOPE_ENABLED"
)

// DefaultClusterCatalogNamespace mirrors pkg/controller/team.SystemNamespace
// (arch §4 control-plane namespace). Mirrored locally so the webhook binary
// does not drag controller deps in — the team controller does the same in
// reverse for its own local mirrors.
const DefaultClusterCatalogNamespace = "ksquad-system"

// DefaultPlatformConfig is the zero-config posture: standard control-plane
// namespace, cluster scope off.
func DefaultPlatformConfig() PlatformConfig {
	return PlatformConfig{ClusterCatalogNamespace: DefaultClusterCatalogNamespace}
}

// PlatformConfigFromEnv reads the deployment-injected configuration,
// falling back to defaults for unset values.
func PlatformConfigFromEnv() PlatformConfig {
	cfg := DefaultPlatformConfig()
	if ns := strings.TrimSpace(os.Getenv(EnvClusterCatalogNamespace)); ns != "" {
		cfg.ClusterCatalogNamespace = ns
	}
	cfg.AllowClusterScope = isTrueEnv(os.Getenv(EnvAllowClusterScope))
	return cfg
}

// WithDefaults fills the zero value's blanks so inline-constructed
// validators (tests, non-standard wiring) behave like the env-loaded ones.
func (p PlatformConfig) WithDefaults() PlatformConfig {
	if strings.TrimSpace(p.ClusterCatalogNamespace) == "" {
		p.ClusterCatalogNamespace = DefaultClusterCatalogNamespace
	}
	return p
}

// IsClusterCatalog reports whether namespace holds the authority catalog.
func (p PlatformConfig) IsClusterCatalog(namespace string) bool {
	return namespace == p.WithDefaults().ClusterCatalogNamespace
}

// isTrueEnv accepts only the explicit truthy spellings; anything else —
// including unset — is false. An opt-in that can be tripped by stray
// values like "yes please" is not an opt-in.
func isTrueEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	}
	return false
}
