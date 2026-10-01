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
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/K8squad/K8squad/api/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// builtinSearchServer is a well-formed built-in memory-search MCPServer in the Run
// namespace with a seeded observedTools surface (as the Team provisioner leaves it).
func builtinSearchServer(mutate func(*api.MCPServer)) *api.MCPServer {
	s := &api.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: searchMCPServerName, Namespace: runNS},
		Spec: api.MCPServerSpec{
			Transport: api.MCPTransportStreamableHTTP,
			Endpoint:  "http://ksquad-memory-search." + runNS + ".svc/mcp",
		},
		Status: api.MCPServerStatus{ObservedTools: []string{"work_item_search"}},
	}
	if mutate != nil {
		mutate(s)
	}
	return s
}

// discussionRun is a source=discussion thread-run (the label intake D2 stamps).
func discussionRun() *api.Run {
	r := newRun()
	r.Labels = map[string]string{api.LabelWorkItemSource: "discussion"}
	return r
}

// ISI-5276: an ordinary (ungranted, non-discussion) ticket Run auto-injects the
// built-in search endpoint — no capability grant and no MCPRefs required. This is
// the "both run types" promise on the ticket side: every run gets work_item_search.
func TestResolveMCPSearchAutoInjectedForTicketRun(t *testing.T) {
	search := builtinSearchServer(nil)
	eps, servers, err := ResolveMCP(context.Background(), capClient(t, search), newRun(), &Requirements{}, GrantSet{})
	require.NoError(t, err)
	require.Len(t, eps, 1)
	assert.Equal(t, searchMCPServerName, eps[0].Name)
	assert.ElementsMatch(t, []string{"work_item_search"}, eps[0].AllowTools)
	require.Len(t, servers, 1)
	assert.Equal(t, searchMCPServerName, servers[0].Name)
}

// ISI-5276: a source=discussion run auto-injects BOTH the discussion endpoint and
// the search endpoint — the "both run types" promise on the discussion side.
func TestResolveMCPSearchAutoInjectedForDiscussionRun(t *testing.T) {
	search := builtinSearchServer(nil)
	discussion := &api.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: discussionMCPServerName, Namespace: runNS},
		Spec: api.MCPServerSpec{
			Transport: api.MCPTransportStreamableHTTP,
			Endpoint:  "http://ksquad-memory-discussion." + runNS + ".svc/mcp",
		},
		Status: api.MCPServerStatus{ObservedTools: []string{"discussion_search", "discussion_post"}},
	}
	eps, _, err := ResolveMCP(context.Background(), capClient(t, search, discussion), discussionRun(), &Requirements{}, GrantSet{})
	require.NoError(t, err)
	names := map[string]bool{}
	for _, ep := range eps {
		names[ep.Name] = true
	}
	assert.True(t, names[searchMCPServerName], "search endpoint must be injected for a discussion run")
	assert.True(t, names[discussionMCPServerName], "discussion endpoint must be injected for a discussion run")
}

// ISI-5276: the search injection is FAIL-OPEN — a Run whose namespace has no
// built-in search server (rollout gap, or deployment with it disabled) assembles
// WITHOUT the tool rather than terminal-failing. Contrast the authoring/discussion
// injections, which fail closed because those runs are minted for that capability.
func TestResolveMCPSearchMissingIsFailOpen(t *testing.T) {
	eps, servers, err := ResolveMCP(context.Background(), capClient(t), newRun(), &Requirements{}, GrantSet{})
	require.NoError(t, err)
	assert.Empty(t, eps)
	assert.Empty(t, servers)
}

// ISI-5276: fail-open also when the built-in surfaced no tools (empty
// observedTools) — the optional search tool drops rather than failing the run.
func TestResolveMCPSearchEmptyObservedIsFailOpen(t *testing.T) {
	search := builtinSearchServer(func(s *api.MCPServer) { s.Status.ObservedTools = nil })
	eps, _, err := ResolveMCP(context.Background(), capClient(t, search), newRun(), &Requirements{}, GrantSet{})
	require.NoError(t, err)
	assert.Empty(t, eps)
}

// ISI-5276: if a project ALSO references the built-in search server via MCPRefs,
// the gate must not double-inject — the built-in name is reserved.
func TestResolveMCPSearchNoDoubleInjectionWhenReferenced(t *testing.T) {
	search := builtinSearchServer(nil)
	reqs := &Requirements{MCPRefs: []api.ObjectRef{{Name: searchMCPServerName}}}
	eps, servers, err := ResolveMCP(context.Background(), capClient(t, search), newRun(), reqs, GrantSet{})
	require.NoError(t, err)
	require.Len(t, eps, 1)
	assert.Equal(t, searchMCPServerName, eps[0].Name)
	require.Len(t, servers, 1)
}
