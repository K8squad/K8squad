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

package capability

import (
	"testing"

	"github.com/stretchr/testify/assert"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/controller/team"
)

// TestDiscussionServerNamePinnedToProvisioner pins the D3 gate's built-in
// discussion server name to the D1 provisioner's canonical constant. If the
// provisioner ever renames the MCPServer this fails loudly instead of the gate
// silently looking up a server that no longer exists (deny-by-default ⇒ every
// source=discussion thread-run would fail assembly without a clear error).
func TestDiscussionServerNamePinnedToProvisioner(t *testing.T) {
	assert.Equal(t, team.DiscussionMCPServerName, discussionMCPServerName)
}

// TestSearchServerNamePinnedToProvisioner pins the ISI-5276 search-endpoint
// injection's built-in server name to the provisioner's canonical constant, so a
// rename on either side fails here rather than silently looking up a server that
// no longer exists (the injection would then always fail-open to no tool).
func TestSearchServerNamePinnedToProvisioner(t *testing.T) {
	assert.Equal(t, team.SearchMCPServerName, searchMCPServerName)
}

// TestDiscussionAllowSetPinnedToMemoryConstants pins the D3 gate's discussion
// allow-set check to the compiled-in memory constants so name drift fails here
// rather than silently letting through zero tools.
func TestDiscussionAllowSetPinnedToMemoryConstants(t *testing.T) {
	assert.Contains(t, discussionAllowedTools, "discussion_search")
	assert.Contains(t, discussionAllowedTools, "discussion_post")
	assert.Len(t, discussionAllowedTools, 2)
}

// TestWorkItemSourceLabelPinnedToAPI pins the isDiscussionRun label key to the
// api/v1alpha1 constant so intake (D2) and capability (D3) can never drift.
func TestWorkItemSourceLabelPinnedToAPI(t *testing.T) {
	assert.Equal(t, api.LabelWorkItemSource, "ksquad.io/work-item-source")
}
