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

package mcpserver

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ISI-3296: sanitizeRemoteText strips control characters (so a hostile
// endpoint cannot forge multi-line or control-char status text) and caps
// the length at maxRemoteErrText runes.
func TestSanitizeRemoteText(t *testing.T) {
	t.Run("strips control characters", func(t *testing.T) {
		assert.Equal(t, "bad text", sanitizeRemoteText("bad\x00\x1f\r\n text\x7f"))
	})
	t.Run("passes short clean text through", func(t *testing.T) {
		assert.Equal(t, "server shutting down", sanitizeRemoteText("server shutting down"))
	})
	t.Run("truncates long text at the cap", func(t *testing.T) {
		got := sanitizeRemoteText(strings.Repeat("x", 10_000))
		assert.Len(t, got, maxRemoteErrText+len("…[truncated]"))
		assert.True(t, strings.HasPrefix(got, strings.Repeat("x", maxRemoteErrText)))
	})
	t.Run("text at the cap is not marked truncated", func(t *testing.T) {
		got := sanitizeRemoteText(strings.Repeat("y", maxRemoteErrText))
		assert.Equal(t, strings.Repeat("y", maxRemoteErrText), got)
	})
	t.Run("count is runes, truncation stays valid UTF-8", func(t *testing.T) {
		got := sanitizeRemoteText(strings.Repeat("é", 5_000))
		assert.True(t, utf8.ValidString(got))
		assert.Equal(t, maxRemoteErrText, utf8.RuneCountInString(strings.TrimSuffix(got, "…[truncated]")))
	})
}

// ISI-3296: a hostile endpoint's JSON-RPC error.message must not land
// verbatim in the error text that flows into MCPServer conditions — it
// arrives control-stripped and length-capped instead.
func TestDiscoverToolsSanitizesJSONRPCError(t *testing.T) {
	hostile := strings.Repeat("x", 600) + "\r\ninjected-status-line Bearer leaked-token"
	double := newMCPTestDouble(t, nil)
	double.rpcErr = hostile

	server := httpMCPServer("hostile", double.URL, nil)
	prober := &StreamableHTTPProber{}
	_, err := prober.DiscoverTools(context.Background(), server, "sekrit")
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "JSON-RPC error -32000")
	assert.NotContains(t, msg, "injected-status-line")
	assert.NotContains(t, msg, "leaked-token")
	assert.NotContains(t, msg, "\r")
	assert.NotContains(t, msg, "\n")
	assert.LessOrEqual(t, strings.Count(msg, "x"), maxRemoteErrText)
}
