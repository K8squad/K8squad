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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// RunRequirements is a Run's collected toolchain demand: the union of its
// agents' skills' spec.requires.toolchains refs (arch §5.3.4), deduped
// with first-seen order and per-ref provenance for actionable errors.
type RunRequirements struct {
	// Refs are the unique name@version strings the Run's skills require.
	Refs []string

	// Sources maps each ref to the first skill (ns/name) that required
	// it — denial messages name the demanding skill, not just the ref.
	Sources map[string]string
}

// RefsForRun walks Run → Agents → Skills and collects the toolchain refs.
// Missing agents/skills are NOT errors here: the story 1.3 cross-ref
// guards already reject dangling Run agents and Agent skillRefs at
// admission, and the renderer only needs the toolchain demand of what
// resolves. Read failures are errors (fail-closed).
func (r *Resolver) RefsForRun(ctx context.Context, run *api.Run) (*RunRequirements, error) {
	reqs := &RunRequirements{Sources: map[string]string{}}
	seenSkills := map[string]bool{}

	for _, agentRef := range run.Spec.Agents {
		agentNS := agentRef.Namespace
		if agentNS == "" {
			agentNS = run.Namespace
		}
		var agent api.Agent
		if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: agentNS, Name: agentRef.Name}, &agent); err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("read agent %s/%s for toolchain resolution (fail-closed): %w", agentNS, agentRef.Name, err)
		}

		for _, skillRef := range agent.Spec.SkillRefs {
			skillNS := skillRef.Namespace
			if skillNS == "" {
				skillNS = agent.Namespace
			}
			skillKey := skillNS + "/" + skillRef.Name
			if seenSkills[skillKey] {
				continue
			}
			var skill api.Skill
			if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: skillNS, Name: skillRef.Name}, &skill); err != nil {
				if isNotFound(err) {
					continue
				}
				return nil, fmt.Errorf("read skill %s for toolchain resolution (fail-closed): %w", skillKey, err)
			}
			seenSkills[skillKey] = true

			for _, ref := range skill.Spec.Requires.Toolchains {
				if _, seen := reqs.Sources[ref]; !seen {
					reqs.Refs = append(reqs.Refs, ref)
					reqs.Sources[ref] = skillKey
				}
			}
		}
	}
	return reqs, nil
}

// DetailsFor renders the provenance suffix for the Run being resolved —
// the shared "who demanded this" context denial messages carry.
func DetailsFor(run *api.Run) string {
	if run == nil {
		return ""
	}
	return fmt.Sprintf("required via run %s/%s skills", run.Namespace, run.Name)
}

func isNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}
