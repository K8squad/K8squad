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

// Package roleprompt resolves a Role's behavior prompt (Role.Spec.PromptRef)
// into the text the context assembler injects at the top of a Run's system
// context (ISI-5223, orchestration MVP, parent ISI-5220).
//
// The mechanism to author + delegate sub-tickets already exists (the ADR-0024
// authoring lane: work_item_create / work_item_assign), but nothing ever
// dereferenced Role.Spec.PromptRef, so a coordinator role's orchestration
// instructions never reached the agent and it behaved like an IC. This package
// closes that gap:
//
//   - PromptRef names a ConfigMap holding the role's prompt text. When present,
//     it wins — an operator can edit a role's behavior with a ConfigMap edit,
//     no rebuild (the ADR-0024 §3 config-not-code posture).
//   - When the ConfigMap is absent or empty AND the Role is a coordinator, the
//     built-in DefaultCoordinatorPrompt is used, so "assign a ticket to the PM
//     and it decomposes + delegates" works out of the box, before any operator
//     applies a prompt ConfigMap.
//   - A non-coordinator role with no prompt ConfigMap resolves to "" — no
//     behavior prompt is injected (unchanged behavior for ICs).
package roleprompt

import (
	"context"
	_ "embed"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// DefaultCoordinatorPrompt is the built-in orchestration prompt for coordinator
// roles (Role.Spec.Coordinator=true) that carry no prompt ConfigMap. It is the
// canonical source of the manager delegation behavior; config/roles/
// role-manager-prompt.yaml carries the same text as an editable ConfigMap
// override (keep the two in sync — the ConfigMap wins when applied).
//
//go:embed coordinator_prompt.md
var DefaultCoordinatorPrompt string

// promptConfigMapKeys are the ConfigMap data keys checked, in order, for the
// prompt text. A single-key ConfigMap is accepted regardless of key name.
var promptConfigMapKeys = []string{"prompt.md", "prompt"}

// Resolve returns the behavior prompt text for role, dereferencing
// role.Spec.PromptRef against a ConfigMap in defaultNS (or the ref's own
// namespace when set). Resolution:
//
//   - nil role, or a role whose PromptRef ConfigMap is absent/empty and which
//     is NOT a coordinator, resolves to "" (no prompt injected);
//   - a present, non-empty PromptRef ConfigMap resolves to its content (an
//     operator override always wins);
//   - a coordinator role with no usable ConfigMap resolves to the built-in
//     DefaultCoordinatorPrompt (behavior on by default).
//
// It is fail-closed on a genuine read error (not NotFound): the caller
// requeues rather than dispatching a run whose role behavior could not be
// resolved. A missing ConfigMap is NOT an error — it is the built-in-default /
// deny-by-default path.
func Resolve(ctx context.Context, reader client.Reader, role *api.Role, defaultNS string) (string, error) {
	if role == nil {
		return "", nil
	}
	ref := role.Spec.PromptRef
	if ref.Name != "" {
		ns := ref.Namespace
		if ns == "" {
			ns = defaultNS
		}
		var cm corev1.ConfigMap
		err := reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &cm)
		switch {
		case err == nil:
			if txt := promptFromConfigMap(&cm); txt != "" {
				return txt, nil
			}
			// Present but no usable prompt text: fall through to the built-in
			// default for coordinators rather than injecting an empty prompt.
		case apierrors.IsNotFound(err):
			// No backing ConfigMap yet: fall through to the built-in default.
		default:
			return "", fmt.Errorf("roleprompt: read prompt ConfigMap %s/%s for role %s: %w", ns, ref.Name, role.Name, err)
		}
	}
	if role.Spec.Coordinator {
		return DefaultCoordinatorPrompt, nil
	}
	return "", nil
}

// promptFromConfigMap extracts the prompt text from a ConfigMap: the first of
// promptConfigMapKeys that is set, else the sole key when the ConfigMap has
// exactly one, else "".
func promptFromConfigMap(cm *corev1.ConfigMap) string {
	for _, k := range promptConfigMapKeys {
		if v, ok := cm.Data[k]; ok && v != "" {
			return v
		}
	}
	if len(cm.Data) == 1 {
		for _, v := range cm.Data {
			return v
		}
	}
	return ""
}
