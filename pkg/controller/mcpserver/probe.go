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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// MCP protocol constants (spike A.2 / MCP spec):
//   - one endpoint; POST carries JSON-RPC 2.0 messages;
//   - the server issues Mcp-Session-Id at initialize, which the client MUST
//     echo afterwards; a 404 means re-initialize;
//   - subsequent requests MUST carry MCP-Protocol-Version or the server
//     assumes 2025-03-26;
//   - the client SHOULD DELETE the session when done.
const (
	// mcpProtocolVersion is the protocol version the operator probe speaks.
	mcpProtocolVersion = "2025-06-18"

	sessionHeader    = "Mcp-Session-Id"
	protocolHeader   = "MCP-Protocol-Version"
	authorizeHeader  = "Authorization"
	bearerPrefix     = "Bearer "
	probeContentType = "application/json"
	probeAccept      = "application/json, text/event-stream"
	maxProbeBody     = 1 << 20 // 1 MiB: a tools/list result never exceeds this

	// maxRemoteErrText caps remote-controlled error text (JSON-RPC
	// error.message, HTTP reason phrases) before it can surface in
	// MCPServer condition messages (ADR-045 hygiene: truncate + strip
	// control chars).
	maxRemoteErrText = 256
)

// sanitizeRemoteText makes remote-controlled text safe for conditions:
// control characters are stripped (newlines included, so a hostile
// endpoint cannot forge multi-line status text) and the length is capped
// at maxRemoteErrText runes with a truncation marker. Everything after
// the cap — e.g. a credential the endpoint may reflect back — is dropped.
func sanitizeRemoteText(s string) string {
	var b strings.Builder
	count := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if count >= maxRemoteErrText {
			b.WriteString("…[truncated]")
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

// httpTimeoutClient builds the default probe client.
func httpTimeoutClient() *http.Client {
	return &http.Client{Timeout: httpProbeTimeout}
}

// jsonrpcRequest is one JSON-RPC 2.0 call.
type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// jsonrpcNotification is one JSON-RPC 2.0 notification (no id).
type jsonrpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
}

// jsonrpcResponse is the reply envelope.
type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// initializeResult carries the negotiated protocol version.
type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
}

// toolsListResult carries the discovered tool surface.
type toolsListResult struct {
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

// StreamableHTTPProber performs the MCP handshake against a
// streamable-http MCPServer from the control plane (ADR-042 option a).
// The credential stays in-memory: it is used for the Authorization header
// and never logged or written to status (ADR-045).
type StreamableHTTPProber struct {
	// Client must set a Timeout; one probe is initialize+list+delete.
	Client *http.Client
}

// DiscoverTools implements HTTPProber.
func (p *StreamableHTTPProber) DiscoverTools(ctx context.Context, server *ksquadv1alpha1.MCPServer, credential string) ([]string, error) {
	if p.Client == nil {
		p.Client = httpTimeoutClient()
	}
	endpoint := server.Spec.Endpoint
	if endpoint == "" {
		return nil, fmt.Errorf("spec.endpoint is empty for transport %q", server.Spec.Transport)
	}

	session, negotiated, err := p.initialize(ctx, endpoint, server, credential)
	if err != nil {
		// Error text carries method + sanitized status/remote text only —
		// never header values, so credential material cannot leak (A3 AC5;
		// remote text is capped + control-stripped per ADR-045, ISI-3296).
		return nil, err
	}
	defer func() {
		if session == "" {
			return
		}
		deleteCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = p.call(deleteCtx, endpoint, session, negotiated, server, credential,
			http.MethodDelete, nil)
	}()

	raw, err := p.call(ctx, endpoint, session, negotiated, server, credential,
		http.MethodPost, &jsonrpcRequest{JSONRPC: "2.0", ID: 2, Method: "tools/list"})
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	var list toolsListResult
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("tools/list: unparsable result: %w", err)
	}
	names := make([]string, 0, len(list.Tools))
	for _, t := range list.Tools {
		if t.Name != "" {
			names = append(names, t.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// initialize performs the MCP initialize handshake and the mandatory
// initialized notification, returning the session id (when issued) and the
// negotiated protocol version.
func (p *StreamableHTTPProber) initialize(ctx context.Context, endpoint string, server *ksquadv1alpha1.MCPServer, credential string) (session, negotiated string, err error) {
	initReq := &jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "ksquad-operator",
				"version": "v1alpha1",
			},
		},
	}
	raw, session, err := p.callWithSession(ctx, endpoint, "", "", server, credential, http.MethodPost, initReq)
	if err != nil {
		return "", "", fmt.Errorf("initialize: %w", err)
	}
	var init initializeResult
	if err := json.Unmarshal(raw, &init); err != nil {
		return "", "", fmt.Errorf("initialize: unparsable result: %w", err)
	}
	negotiated = init.ProtocolVersion
	if negotiated == "" {
		negotiated = mcpProtocolVersion
	}

	// The spec requires the initialized notification before any request.
	if _, _, err := p.callWithSession(ctx, endpoint, session, negotiated, server, credential,
		http.MethodPost, &jsonrpcNotification{JSONRPC: "2.0", Method: "notifications/initialized"}); err != nil {
		return "", "", fmt.Errorf("initialized notification: %w", err)
	}
	return session, negotiated, nil
}

// call issues one request and decodes the JSON-RPC envelope (POST). DELETE
// ignores the body.
func (p *StreamableHTTPProber) call(ctx context.Context, endpoint, session, negotiated string, server *ksquadv1alpha1.MCPServer, credential, method string, payload any) ([]byte, error) {
	raw, _, err := p.callWithSession(ctx, endpoint, session, negotiated, server, credential, method, payload)
	return raw, err
}

// callWithSession issues one HTTP request against the endpoint with the
// static spec headers plus session/protocol/auth headers. It returns the
// JSON-RPC result for calls, or the raw (empty) body for notifications and
// DELETE, along with any session id the server issued.
func (p *StreamableHTTPProber) callWithSession(ctx context.Context, endpoint, session, negotiated string, server *ksquadv1alpha1.MCPServer, credential, method string, payload any) ([]byte, string, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, "", err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", probeContentType)
	req.Header.Set("Accept", probeAccept)
	for k, v := range server.Spec.Headers {
		req.Header.Set(k, v) // static, NON-secret (webhook-enforced)
	}
	if session != "" {
		req.Header.Set(sessionHeader, session)
	}
	if negotiated != "" {
		req.Header.Set(protocolHeader, negotiated)
	}
	if credential != "" {
		req.Header.Set(authorizeHeader, bearerPrefix+credential)
	}

	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if method == http.MethodDelete {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProbeBody))
		if resp.StatusCode >= 400 {
			return nil, "", fmt.Errorf("DELETE %s", sanitizeRemoteText(resp.Status))
		}
		return nil, "", nil
	}

	// A 404 on a session'd request means the session died: re-init needed.
	if resp.StatusCode == http.StatusNotFound && session != "" {
		return nil, "", fmt.Errorf("session expired (404): re-initialize")
	}
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("HTTP %s", sanitizeRemoteText(resp.Status))
	}

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
	if err != nil {
		return nil, "", err
	}
	issued := resp.Header.Get(sessionHeader)

	// Notifications carry no reply envelope.
	if _, isNotification := payload.(*jsonrpcNotification); isNotification {
		return nil, issued, nil
	}

	// Streamable-http replies may arrive as SSE frames; accept both by
	// scanning for the data: line when the content type says so.
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		respBytes = sseDataPayload(respBytes)
		if respBytes == nil {
			return nil, issued, fmt.Errorf("SSE reply carried no data frame")
		}
	}

	var envelope jsonrpcResponse
	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		return nil, issued, fmt.Errorf("unparsable reply: %w", err)
	}
	if envelope.Error != nil {
		// error.message is remote-controlled text: sanitize before it can
		// reach condition messages (ADR-045 hygiene; ISI-3296).
		return nil, issued, fmt.Errorf("JSON-RPC error %d: %s",
			envelope.Error.Code, sanitizeRemoteText(envelope.Error.Message))
	}
	return envelope.Result, issued, nil
}

// sseDataPayload extracts the JSON payload from an SSE data frame.
func sseDataPayload(b []byte) []byte {
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(line, []byte("data:")) {
			return bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		}
	}
	return nil
}
