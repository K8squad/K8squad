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

// project.go — the per-Project workspace PVC naming contract (ISI-4127,
// arch §5.1/§9.4). One PVC per Project, shared by the team's agent runtime
// pods; the name is derived in exactly one place so the provisioning
// controller (pkg/controller/projectpvc), the warm-pool classifier
// (pkg/controller/rundrive.SpecClassifier) and any future reader/mounter
// all address the same claim.
package workspace

import (
	"fmt"
	"hash/fnv"
)

// projectPVCPrefix prefixes the per-Project workspace claim, keeping it
// visually and programmatically distinct from the per-Run claims
// ("workspace-<run>", manager.go).
const projectPVCPrefix = "workspace-project-"

// LabelProject identifies the Project that owns a per-Project workspace PVC.
const LabelProject = "k8squad.io/project"

// projectPVCHashLen is the hex length of the fnv-32a suffix appended when a
// Project name is long enough that the plain prefixed name would exceed the
// 63-char DNS-1123 cap — same disambiguation discipline as the per-principal
// partition hash (pkg/sandbox/workspace.go).
const projectPVCHashLen = 8

// maxPVCNameLen is the DNS-1123 label cap for PVC names.
const maxPVCNameLen = 63

// ProjectPVCName returns the deterministic workspace PVC name for a Project:
// "workspace-project-<project>". Project names are already DNS-1123, so the
// plain form is valid whenever it fits; an over-long name is truncated and
// disambiguated with a stable fnv-32a hash of the FULL name (truncation
// alone could collide two Projects sharing a long prefix).
func ProjectPVCName(projectName string) string {
	name := projectPVCPrefix + projectName
	if len(name) <= maxPVCNameLen {
		return name
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(projectName))
	suffix := fmt.Sprintf("%08x", h.Sum32())[:projectPVCHashLen]
	keep := maxPVCNameLen - len(projectPVCPrefix) - projectPVCHashLen - 1
	return fmt.Sprintf("%s%s-%s", projectPVCPrefix, projectName[:keep], suffix)
}
