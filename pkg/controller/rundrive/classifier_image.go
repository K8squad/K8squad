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

package rundrive

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/warmpool"
)

// EnvSandboxImage is the operator env naming the default sandbox image every
// runtime type uses unless a type-specific override applies (M1.2: the
// AgentRuntime → image dimension of the pool key, the ISI-2889 gap).
const EnvSandboxImage = "KSQUAD_SANDBOX_IMAGE"

// EnvSandboxImagePrefix per type is KSQUAD_SANDBOX_IMAGE_<TYPE> with the type
// normalized (uppercase, '-' → '_'): e.g. KSQUAD_SANDBOX_IMAGE_CLAUDE_CODE
// for AgentRuntime type "claude-code".
const EnvSandboxImagePrefix = "KSQUAD_SANDBOX_IMAGE_"

// RuntimeImages resolves the sandbox pod image for an AgentRuntime type —
// the M1.2 answer to SpecClassifier's empty-image gap (ISI-2889 scope cut to
// the image dimension): the operator deployment names one default image
// (KSQUAD_SANDBOX_IMAGE) and may pin per-flavor overrides. Resolution is
// env-driven so clusters with private registries (k8squad-test: 10.0.0.13)
// point at their own mirror without code changes.
type RuntimeImages struct {
	// Default serves every runtime type without a specific override.
	Default string
	// ByType maps AgentRuntime.spec.type → image, from
	// KSQUAD_SANDBOX_IMAGE_<TYPE> env overrides.
	ByType map[string]string
}

// RuntimeImagesFromEnv builds the resolver from the process env.
func RuntimeImagesFromEnv(getenv func(string) string) RuntimeImages {
	imgs := RuntimeImages{ByType: map[string]string{}}
	imgs.Default = getenv(EnvSandboxImage)
	for _, t := range []string{
		api.RuntimeTypeOpenClaw, api.RuntimeTypeClaudeCode, api.RuntimeTypeOpenCode,
		api.RuntimeTypeHermes, api.RuntimeTypeCodex,
	} {
		if v := getenv(envImageKey(t)); v != "" {
			imgs.ByType[t] = v
		}
	}
	return imgs
}

// envImageKey normalizes an AgentRuntime type to its env override name.
func envImageKey(runtimeType string) string {
	return EnvSandboxImagePrefix + strings.ToUpper(strings.ReplaceAll(runtimeType, "-", "_"))
}

// ImageFor resolves the sandbox image for a runtime type: the type-specific
// override first, then the default. Empty means unconfigured — the caller
// (SpecClassifier) turns that into a loud bind error rather than a pod with
// an empty image.
func (r RuntimeImages) ImageFor(runtimeType string) string {
	if v, ok := r.ByType[runtimeType]; ok && v != "" {
		return v
	}
	return r.Default
}

// resolveSandboxImage walks Run → Agents[0] → Agent.spec.runtimeRef →
// AgentRuntime.spec.type and resolves the image through imgs. Errors name the
// dangling hop so a misconfigured graph fails the bind diagnosably.
func resolveSandboxImage(ctx context.Context, reader client.Reader, run *api.Run, imgs RuntimeImages) (string, error) {
	if len(run.Spec.Agents) == 0 {
		return "", fmt.Errorf("run %s/%s has no dispatch agent to resolve a runtime image from", run.Namespace, run.Name)
	}
	ref := run.Spec.Agents[0]
	agentNS := ref.Namespace
	if agentNS == "" {
		agentNS = run.Namespace
	}
	var agent api.Agent
	if err := reader.Get(ctx, client.ObjectKey{Namespace: agentNS, Name: ref.Name}, &agent); err != nil {
		return "", fmt.Errorf("resolve Agent %s/%s for sandbox image: %w", agentNS, ref.Name, err)
	}
	rtRef := agent.Spec.RuntimeRef
	if rtRef.Name == "" {
		return "", fmt.Errorf("agent %s/%s has no runtimeRef to resolve a sandbox image from", agentNS, agent.Name)
	}
	rtNS := rtRef.Namespace
	if rtNS == "" {
		rtNS = agentNS
	}
	var rt api.AgentRuntime
	if err := reader.Get(ctx, client.ObjectKey{Namespace: rtNS, Name: rtRef.Name}, &rt); err != nil {
		return "", fmt.Errorf("resolve AgentRuntime %s/%s for sandbox image: %w", rtNS, rtRef.Name, err)
	}
	image := imgs.ImageFor(rt.Spec.Type)
	if image == "" {
		return "", fmt.Errorf("no sandbox image configured for AgentRuntime type %q (set %s or %s)",
			rt.Spec.Type, envImageKey(rt.Spec.Type), EnvSandboxImage)
	}
	return image, nil
}

// classifySandbox fills the image dimension of key for the matched Run.
// It is the SpecClassifier extension point kept as a helper so the classifier
// stays a single readable function.
func classifySandbox(ctx context.Context, reader client.Reader, run *api.Run, imgs RuntimeImages, key warmpool.PoolKey) (warmpool.PoolKey, error) {
	image, err := resolveSandboxImage(ctx, reader, run, imgs)
	if err != nil {
		return key, err
	}
	key.Image = image
	return key, nil
}
