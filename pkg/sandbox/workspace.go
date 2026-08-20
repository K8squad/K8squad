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

package sandbox

import (
	"fmt"
	"hash/fnv"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// Workspace subpath layout (arch §9.4, story 4.5 B).
//
// The persistent Project workspace PVC (story 4.3) is partitioned per
// principal: every Run mounts ONLY its own principal's partition, so a
// shared Project workspace can never expose one user's source/secrets to
// another agent's Run (the D7 crux). Same-principal reuse is the point of a
// persistent cache (FR-C2) — scoping is cross-principal only, never a
// per-Run reset.
const (
	// WorkspaceCacheDir is the per-principal partition root on the PVC.
	WorkspaceCacheDir = "cache"

	// WorkspaceWorktreeDir holds the per-Run git worktrees (story 4.4 owns
	// the concurrency semantics; 4.5 only partitions it per principal).
	WorkspaceWorktreeDir = "worktrees"

	// principalHashLen is the hex length of the principal-hash suffix —
	// mirrors the 4.1 namespace-hash discipline: a raw principal id can
	// collide after normalization or contain unsafe path characters; the
	// hash disambiguates and a principal rename does not strand the
	// partition (it simply addresses a new one; ops can migrate the old).
	principalHashLen = 8
)

// PrincipalPartition derives the deterministic per-principal PVC partition
// (story 4.5 AC5): cache/<normalized-principal>-<short-hash(principal)>.
//
// Deterministic: same principal → same partition. Collision-safe: distinct
// principals → distinct partitions (stable fnv-32a hash suffix). Path-safe:
// the normalized fragment cannot traverse (`..` collapses to `-`), and the
// result is validated by ValidatePartition for defense in depth.
func PrincipalPartition(principal api.PrincipalRef) string {
	normalized := normalizePrincipal(string(principal))
	const budget = 63 - 1 - principalHashLen // "-" separator before hash
	if len(normalized) > budget {
		normalized = normalized[:budget]
	}
	return fmt.Sprintf("%s/%s-%s", WorkspaceCacheDir, normalized, shortHashPrincipal(string(principal)))
}

// normalizePrincipal lowercases and collapses everything outside [a-z0-9-]
// into a single `-` (emails like "Alice@Corp.com" → "alice-corp-com"). An
// empty result falls back to "principal" so the partition is never empty.
func normalizePrincipal(principal string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(principal) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "principal"
	}
	return out
}

func shortHashPrincipal(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())[:principalHashLen]
}

// ValidatePartition fail-closes on a partition that could escape the
// per-principal directory: it must be relative, cleaned, under the cache
// root, and free of traversal segments. A partition failing this check is a
// construction failure — the mount must not be emitted.
func ValidatePartition(partition string) error {
	clean := path.Clean(partition)
	if path.IsAbs(partition) || clean != partition {
		return &PolicyError{Reason: fmt.Sprintf("workspace partition %q is not a clean relative path", partition)}
	}
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return &PolicyError{Reason: fmt.Sprintf("workspace partition %q escapes the cache root", partition)}
	}
	root, rest := path.Split(clean)
	if path.Clean(root) != WorkspaceCacheDir || rest == "" {
		return &PolicyError{Reason: fmt.Sprintf("workspace partition %q is not rooted under %s/", partition, WorkspaceCacheDir)}
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == "." || seg == ".." {
			return &PolicyError{Reason: fmt.Sprintf("workspace partition %q contains a traversal segment", partition)}
		}
	}
	return nil
}

// WorkspaceVolumeMounts builds the Run's workspace mounts over the shared
// Project PVC: source and build cache BOTH mount through the Run's own
// principal's partition (story 4.5 AC4). A shared per-Project subPath mounted
// across principals is the exfil hole this closes — there is no code path
// that produces one.
//
//	pvcName: the Project workspace PVC (story 4.3)
//	pvcKey:  the subPath parent on the volume the platform allocated (may be
//	         empty when the PVC is dedicated per Project)
func WorkspaceVolumeMounts(pvcName, pvcKey string, principal api.PrincipalRef) ([]corev1.VolumeMount, error) {
	partition := PrincipalPartition(principal)
	if err := ValidatePartition(partition); err != nil {
		return nil, err
	}
	base := pvcKey
	cacheSub := path.Join(base, partition)
	worktreeSub := path.Join(base, WorkspaceWorktreeDir, path.Base(partition))
	return []corev1.VolumeMount{
		{Name: "workspace-source", ReadOnly: false, MountPath: "/workspace/source", SubPath: worktreeSub},
		{Name: "workspace-cache", ReadOnly: false, MountPath: "/workspace/cache", SubPath: cacheSub},
	}, nil
}
