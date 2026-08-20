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

// Package sandbox implements the Story 4.2 RuntimeClass-selection contract
// (arch §9.1) and the Story 4.5 teardown-and-replace + per-principal scoping
// contract (arch §9.3/§9.4): every Run gets its own kernel-isolated sandbox
// pod under gVisor (default) or Kata (opt-in) — never runc for untrusted
// code, never a silent downgrade when the class is missing — a completed
// Run's pod is destroyed and replaced (never reset-and-reused), and the
// persistent Project workspace PVC is partitioned per principal so a Run
// mounts only its own principal's source/cache.
package sandbox

import (
	"context"
	"errors"
	"fmt"

	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// RuntimeClass names (arch §9.1, ISI-2113 ratified): gVisor is the default,
// Kata the high-assurance opt-in, runc trusted-dev-only.
const (
	// ClassGVisor is the gVisor RuntimeClass (syscall-interception default).
	ClassGVisor = "gvisor"
	// ClassKata is the Kata microVM RuntimeClass (high-assurance opt-in).
	ClassKata = "kata"
	// ClassRunc is the shared-kernel runtime — rejected for untrusted code.
	ClassRunc = "runc"
	// DefaultRuntimeClass is the §9.1 ratified default (supersedes the epic
	// text's "Kata default / gVisor fallback" placeholder — the ISI-2113
	// confirming gate is authoritative).
	DefaultRuntimeClass = ClassGVisor

	// TrustedDevAnnotation is the explicit, audited, non-default opt-out that
	// admits runc for a first-party trusted dev sandbox (story 4.2 §2). It is
	// deliberately an annotation, not a spec field: setting it must be a
	// visible, greppable act.
	TrustedDevAnnotation = "ksquad.io/trusted-dev"
)

// approvedClasses is the allowlist of isolation runtimes admitted for
// untrusted code (story 4.2 AC2 — allowlist, not denylist: any
// installed-but-not-approved class, e.g. an operator-added weak runtime, is
// rejected exactly like runc).
var approvedClasses = map[string]struct{}{
	ClassGVisor: {},
	ClassKata:   {},
}

// Selection is the RuntimeClass precedence input (story 4.2 §1): most
// specific wins, defaulting to gVisor.
type Selection struct {
	// RunPolicy is Run.spec.sandboxPolicy.runtimeClass.
	RunPolicy string
	// RoleHint is Role.runtimeClassHint.
	RoleHint string
	// PoolClass is SandboxPool.runtimeClass.
	PoolClass string
}

// ResolveRuntimeClass applies the §9.1 precedence chain
// Run.spec.sandboxPolicy.runtimeClass → Role.runtimeClassHint →
// SandboxPool.runtimeClass → gvisor (story 4.2 AC1).
func ResolveRuntimeClass(sel Selection) string {
	switch {
	case sel.RunPolicy != "":
		return sel.RunPolicy
	case sel.RoleHint != "":
		return sel.RoleHint
	case sel.PoolClass != "":
		return sel.PoolClass
	default:
		return DefaultRuntimeClass
	}
}

// IsApprovedClass reports whether class is an isolation runtime admitted for
// untrusted code (gVisor or Kata).
func IsApprovedClass(class string) bool {
	_, ok := approvedClasses[class]
	return ok
}

// TrustedDev reports whether obj carries the explicit trusted-dev escape
// (annotation "ksquad.io/trusted-dev": "true").
func TrustedDev(obj metav1.Object) bool {
	return obj.GetAnnotations()[TrustedDevAnnotation] == "true"
}

// AdmitRuntimeClass enforces the AD-3 crux (story 4.2 AC2, fail-closed): a
// resolved class of runc, the empty/node-default runtime, or any
// non-approved class is rejected for untrusted code. The ONLY escape is the
// explicit trustedDev flag. "No runtimeClassName set" is a rejection, not a
// pass — an empty field silently runs on the node default runtime, which is
// the exact hole this closes.
func AdmitRuntimeClass(class string, trustedDev bool) error {
	effective := class
	if effective == "" {
		// Empty means "node default runtime" = runc-equivalent.
		effective = ClassRunc
	}
	if IsApprovedClass(effective) {
		return nil
	}
	if trustedDev {
		return nil
	}
	return &PolicyError{
		Class:     class,
		TrustedDev: trustedDev,
		Reason: fmt.Sprintf("runtime class %q is not an approved isolation runtime for untrusted code (approved: gvisor, kata); "+
			"set the explicit %s=true escape only for trusted first-party dev sandboxes", effective, TrustedDevAnnotation),
	}
}

// PolicyError is a fail-closed sandbox-policy violation.
type PolicyError struct {
	Class      string
	TrustedDev bool
	Reason     string
}

func (e *PolicyError) Error() string { return e.Reason }

// IsPolicyError reports whether err is a sandbox-policy (fail-closed) error.
func IsPolicyError(err error) bool {
	var pe *PolicyError
	return errors.As(err, &pe)
}

// RuntimeClassUnavailableError is raised when the selected RuntimeClass does
// not exist on the cluster (story 4.2 AC4): the operator fail-closes and
// NEVER downgrades to runc / the node default to "keep the Run moving".
type RuntimeClassUnavailableError struct {
	Class string
}

func (e *RuntimeClassUnavailableError) Error() string {
	return fmt.Sprintf("runtimeclass %q does not exist on the cluster (documented prerequisite, S1); failing the Run closed rather than downgrading isolation", e.Class)
}

// IsRuntimeClassUnavailable reports whether err is a missing-RuntimeClass
// fail-closed error.
func IsRuntimeClassUnavailable(err error) bool {
	var rcu *RuntimeClassUnavailableError
	return errors.As(err, &rcu)
}

// EnsureRuntimeClassExists verifies a RuntimeClass object of the resolved
// name exists before any pod is bound/created (story 4.2 §3). Absence is a
// RuntimeClassUnavailableError — the caller records a
// RuntimeClassUnavailable condition on the Run and creates no pod. Installing
// the runtime handlers themselves is an out-of-band cluster prerequisite
// (ISI-2294 posture); this check only selects among installed classes.
func EnsureRuntimeClassExists(ctx context.Context, c client.Client, class string) error {
	var rc nodev1.RuntimeClass
	err := c.Get(ctx, client.ObjectKey{Name: class}, &rc)
	if apierrors.IsNotFound(err) {
		return &RuntimeClassUnavailableError{Class: class}
	}
	if err != nil {
		return fmt.Errorf("check runtimeclass %q: %w", class, err)
	}
	return nil
}

// SelectRuntimeClass is the full §9.1 selection pipeline for a Run: resolve
// by precedence, admit against the untrusted-code allowlist, and verify the
// class exists on the cluster. It returns the admitted class name (non-empty)
// or a fail-closed error. The trustedDev flag comes from the Run's
// TrustedDevAnnotation escape (story 4.2 §2).
func SelectRuntimeClass(ctx context.Context, c client.Client, run *api.Run, sel Selection) (string, error) {
	class := ResolveRuntimeClass(sel)
	if err := AdmitRuntimeClass(class, TrustedDev(run)); err != nil {
		return "", err
	}
	if class == "" {
		// trustedDev + empty selection: pin the concrete node-default name
		// so the emitted pod still carries an explicit runtimeClassName.
		class = ClassRunc
	}
	if err := EnsureRuntimeClassExists(ctx, c, class); err != nil {
		return "", err
	}
	return class, nil
}
