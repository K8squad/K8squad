/*
Copyright 2026 The K8sAgentMesh Authors.

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

package mesh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
)

// The CrewAI wire contract, locked by the ISI-6547 spike against the pinned
// stack (crewai[a2a] 1.15.28 → a2a-sdk ~=0.3.10, Google A2A protocol 0.3.0;
// verified 2026-10-23, see docs/crewai-integration.md):
//
//   - Transport is the JSON-RPC binding. CrewAI's ServerTransportConfig has
//     jsonrpc on a default factory (rpc_path "/a2a", card at
//     /.well-known/agent-card.json) while grpc and http_json default to None —
//     JSON-RPC is the only binding a stock CrewAI A2A server is guaranteed to
//     serve, so the design's HTTP+JSON preference loses to guaranteed presence.
//   - Method names are the 0.3.x dashed forms: message/send, message/stream,
//     tasks/get, tasks/cancel (PascalCase + REST arrive at protocol 1.0).
//   - message/stream IS the submission: one POST returns text/event-stream
//     whose data frames are JSON-RPC envelopes ({"jsonrpc","id","result"})
//     whose result is the 0.3.x Task snapshot (kind "task") followed by
//     status-update / artifact-update / message frames until terminal.
//   - Server auth defaults to SimpleTokenAuth reading the AUTH_TOKEN env var:
//     the router sends it as a Bearer header when the host wires one in.
//   - Task ids are SERVER-generated and message/send dedup on messageId is a
//     spec MAY CrewAI does not document — so C1 (re-route reattaches, never
//     double-executes) is closed adapter-side: an in-flight map plus a
//     bounded completed map, with tasks/get reattach replacing any re-send.
const (
	// crewaiRPCPath is the JSON-RPC endpoint path on the crew's A2A server
	// (CrewAI JSONRPCServerConfig.rpc_path default).
	crewaiRPCPath = "/a2a"
	// crewaiReattachPoll is the tasks/get poll interval used while reattached
	// to an in-flight task (C1 re-route, or an SSE tear before terminal).
	crewaiReattachPoll = 500 * time.Millisecond
	// crewaiCompletedTrack bounds the completed-task map (re-route on a
	// terminal task must answer the terminal status, never re-send).
	crewaiCompletedTrack = 1024
	// crewaiJSONRPCID is the client id stamped on every JSON-RPC request the
	// router issues (one outstanding call per HTTP request; no correlation
	// ambiguity on this wire).
	crewaiJSONRPCID = 1
)

// crewaiMethod* are the 0.3.x JSON-RPC method names the router speaks.
const (
	crewaiMethodStream = "message/stream"
	crewaiMethodSend   = "message/send"
	crewaiMethodGet    = "tasks/get"
	crewaiMethodCancel = "tasks/cancel"
)

// NewCrewaiRouter returns the CrewAI Router: it translates the module a2a.Task
// wire (V1 submit / V2 event stream) onto the crew's Google A2A 0.3.x
// JSON-RPC+SSE surface, and back onto module a2a.Event frames. It is the R11
// seam — the whole Google wire lives inside this type, so the mesh keeps
// speaking only the module wire.
//
// A nil client uses a default client with NO timeout: message/stream's
// response body IS the task's live SSE stream and stays open until the task
// settles, so cancellation rides the Route context, not a client deadline.
// token is the AUTH_TOKEN the crew's SimpleTokenAuth expects (empty = the
// no-auth lane); it is sent as a Bearer header and never logged.
func NewCrewaiRouter(client *http.Client, token string) Router {
	if client == nil {
		client = &http.Client{}
	}
	return &crewaiRouter{
		client: client,
		token:  token,
	}
}

// crewaiRouter is the CrewAI/Google-A2A translation Router.
type crewaiRouter struct {
	client *http.Client
	token  string

	mu        sync.Mutex
	inflight  map[string]*crewTaskState // a2a_task_id → live-task bookkeeping
	completed map[string]*crewTaskState // bounded terminal set (C1 re-route)
}

// crewTaskState is everything the router remembers about one mapped task: the
// crew-side identifiers, the module-side seq cursor (C4: monotonic, gap-free,
// continuing across a reattach), the last status mirrored to the follower, and
// the artifact ids already emitted (poll reattach must not double-report).
// It is shared by every stream that ever tailed the task (the first SSE tail
// and any later reattach), so all field access goes through mu.
type crewTaskState struct {
	mu        sync.Mutex
	googleID  string
	contextID string
	lastSeq   uint64
	status    a2a.Status
	terminal  bool
	seenArt   map[string]bool
}

// adoptIDs records the crew-side task/context ids (server-generated; the
// first frame that carries them wins).
func (st *crewTaskState) adoptIDs(g, ctx string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if g != "" {
		st.googleID = g
		st.contextID = ctx
	}
}

// snapshot returns the ids and terminal flag under the lock.
func (st *crewTaskState) snapshot() (googleID string, terminal bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.googleID, st.terminal
}

// currentStatus returns the last status mirrored to a follower.
func (st *crewTaskState) currentStatus() a2a.Status {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.status
}

// settleStatus records a status observation; markTerminal folds a terminal
// condition in and reports whether the task is now terminal.
func (st *crewTaskState) settleStatus(s a2a.Status, terminalCond bool) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.status = s
	st.terminal = st.terminal || terminalCond
	return st.terminal
}

// claimArtifact reports whether the artifact is new (and remembers it).
func (st *crewTaskState) claimArtifact(id string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if id == "" || st.seenArt[id] {
		return false
	}
	st.seenArt[id] = true
	return true
}

// nextSeq allocates the next module-side sequence number (C4: monotonic,
// gap-free, continuing across a reattach — the cursor lives on st, not the
// stream, so a re-route picks up where the first tail left off).
func (st *crewTaskState) nextSeq() uint64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.lastSeq++
	return st.lastSeq
}

// Route delivers one module task to the crew serving at endpoint. First
// delivery submits via message/stream and returns its SSE response as the
// Stream. A re-route on the same a2a_task_id NEVER re-sends — it reattaches
// via tasks/get polling (C1; CrewAI does not document task-id dedup, and a
// second message would start a second crew run). A re-route on a task this
// router already saw terminal answers the recorded terminal status.
func (r *crewaiRouter) Route(ctx context.Context, endpoint string, task a2a.Task) (Stream, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("mesh: crewai router requires a non-empty endpoint")
	}
	if task.A2ATaskID == "" {
		return nil, fmt.Errorf("mesh: crewai router requires a non-empty a2a_task_id")
	}

	r.mu.Lock()
	if st, ok := r.completed[task.A2ATaskID]; ok {
		r.mu.Unlock()
		// C1: terminal re-route replays the terminal status, no re-send.
		return newCrewReplayStream(task.A2ATaskID, st), nil
	}
	if st, ok := r.inflight[task.A2ATaskID]; ok {
		r.mu.Unlock()
		return r.reattach(ctx, endpoint, task.A2ATaskID, st)
	}
	st := &crewTaskState{seenArt: map[string]bool{}}
	if r.inflight == nil {
		r.inflight = map[string]*crewTaskState{}
	}
	r.inflight[task.A2ATaskID] = st
	r.mu.Unlock()

	stream, err := r.submit(ctx, endpoint, task, st)
	if err != nil {
		r.mu.Lock()
		// Only drop the entry when the submit itself failed — a submit error
		// means no crew task exists, so a retry may send fresh.
		delete(r.inflight, task.A2ATaskID)
		r.mu.Unlock()
		return nil, err
	}
	return stream, nil
}

// submit POSTs message/stream and returns the SSE tail as a Stream. The SSE
// response is both the submission and the event stream (one send, one stream —
// never a second message for the same task).
func (r *crewaiRouter) submit(ctx context.Context, endpoint string, task a2a.Task, st *crewTaskState) (Stream, error) {
	msg := crewWireMessage{
		Role:      "user",
		Parts:     []crewWirePart{{Kind: "text", Text: crewTaskText(task)}},
		MessageID: task.A2ATaskID,
	}
	if md := crewTaskMetadata(task); len(md) > 0 {
		msg.Metadata = md
	}
	resp, err := r.post(ctx, endpoint, crewaiMethodStream, crewWireSendParams{Message: msg})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if crewIsUnsupportedStreaming(resp.StatusCode, body) {
			// capabilities.streaming=false: fall back to message/send + poll.
			return r.sendAndPoll(ctx, endpoint, task, st, msg)
		}
		return nil, fmt.Errorf("mesh: crewai message/stream: process answered %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		// A non-SSE 200: either an in-body JSON-RPC error envelope (the
		// -32004 unsupported-streaming case) or a server that answered the
		// stream call with a plain Task result. Decode both; the SSE pump
		// would otherwise skip the envelope silently.
		gt, body, derr := crewDecodeTaskBody(resp)
		if derr != nil {
			if crewIsUnsupportedStreaming(http.StatusOK, body) {
				return r.sendAndPoll(ctx, endpoint, task, st, msg)
			}
			return nil, derr
		}
		st.googleID = gt.ID
		st.contextID = gt.ContextID
		s := &crewStream{
			router:   r,
			endpoint: endpoint,
			taskID:   task.A2ATaskID,
			st:       st,
			events:   make(chan a2a.Event),
			quit:     make(chan struct{}),
		}
		go s.pumpPoll(context.WithoutCancel(ctx))
		return s, nil
	}
	s := &crewStream{
		router:   r,
		endpoint: endpoint,
		taskID:   task.A2ATaskID,
		st:       st,
		body:     resp.Body,
		events:   make(chan a2a.Event),
		quit:     make(chan struct{}),
	}
	go s.pumpSSE()
	return s, nil
}

// sendAndPoll is the non-streaming fallback: message/send returns the Task
// synchronously; the poll pump then tails it with tasks/get until terminal.
func (r *crewaiRouter) sendAndPoll(ctx context.Context, endpoint string, task a2a.Task, st *crewTaskState, msg crewWireMessage) (Stream, error) {
	resp, err := r.post(ctx, endpoint, crewaiMethodSend, crewWireSendParams{Message: msg})
	if err != nil {
		return nil, err
	}
	gt, err := crewDecodeTaskResponse(resp)
	if err != nil {
		return nil, err
	}
	st.googleID = gt.ID
	st.contextID = gt.ContextID
	s := &crewStream{
		router:   r,
		endpoint: endpoint,
		taskID:   task.A2ATaskID,
		st:       st,
		events:   make(chan a2a.Event),
		quit:     make(chan struct{}),
	}
	go s.pumpPoll(ctx)
	return s, nil
}

// reattach tails an already in-flight crew task with tasks/get polling. 0.3.x
// has no tasks/subscribe, and re-sending message/stream (or message/send with
// the message's taskId set) is multi-turn semantics on this wire — it would
// trigger a second crew run, exactly what C1 forbids. Polling is the only
// reattach a stock crew serves.
func (r *crewaiRouter) reattach(ctx context.Context, endpoint, taskID string, st *crewTaskState) (Stream, error) {
	s := &crewStream{
		router:   r,
		endpoint: endpoint,
		taskID:   taskID,
		st:       st,
		events:   make(chan a2a.Event),
		quit:     make(chan struct{}),
	}
	go s.pumpPoll(ctx)
	return s, nil
}

// --- JSON-RPC plumbing -------------------------------------------------

// post issues one JSON-RPC request and returns the raw HTTP response.
func (r *crewaiRouter) post(ctx context.Context, endpoint, method string, params any) (*http.Response, error) {
	url := strings.TrimRight(endpoint, "/") + crewaiRPCPath
	payload, err := json.Marshal(crewWireRequest{
		JSONRPC: "2.0",
		ID:      crewaiJSONRPCID,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, fmt.Errorf("mesh: crewai marshal %s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("mesh: crewai build POST %s: %w", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mesh: crewai POST %s: %w", url, err)
	}
	return resp, nil
}

// crewGetTask fetches the crew-side Task by its server id (V3 → tasks/get).
func (r *crewaiRouter) crewGetTask(ctx context.Context, endpoint, googleID string) (*crewWireTask, error) {
	resp, err := r.post(ctx, endpoint, crewaiMethodGet, crewWireTaskIDParams{ID: googleID})
	if err != nil {
		return nil, err
	}
	return crewDecodeTaskResponse(resp)
}

// crewCancelTask cancels the crew-side task (V4 → tasks/cancel). Idempotent
// per the spec (a duplicate cancel is a no-op success), so the best-effort
// Close path may call it freely.
func (r *crewaiRouter) crewCancelTask(ctx context.Context, endpoint, googleID string) error {
	resp, err := r.post(ctx, endpoint, crewaiMethodCancel, crewWireTaskIDParams{ID: googleID})
	if err != nil {
		return err
	}
	_, err = crewDecodeTaskResponse(resp)
	return err
}

// crewIsUnsupportedStreaming reports whether a message/stream rejection
// carries the -32004 UnsupportedOperation error (capabilities.streaming=false)
// — the one failure that legitimately falls back to message/send. a2a-sdk
// answers JSON-RPC errors both as 4xx/5xx statuses and as 200 with an in-body
// error envelope, so the status is no signal: parse the body either way.
func crewIsUnsupportedStreaming(status int, body []byte) bool {
	var e struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Error == nil {
		return false
	}
	return e.Error.Code == -32004
}

// crewDecodeTaskResponse decodes a JSON-RPC response carrying a Task result
// (message/send, tasks/get, tasks/cancel), failing on the error envelope.
func crewDecodeTaskResponse(resp *http.Response) (*crewWireTask, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	closeErr := resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("mesh: crewai read response: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("mesh: crewai close response: %w", closeErr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mesh: crewai task response: process answered %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var env crewWireEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("mesh: crewai decode response: %w", err)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("mesh: crewai JSON-RPC error %d: %s", env.Error.Code, env.Error.Message)
	}
	if env.Result == nil {
		return nil, fmt.Errorf("mesh: crewai response carried no result")
	}
	var t crewWireTask
	if err := json.Unmarshal(env.Result, &t); err != nil {
		return nil, fmt.Errorf("mesh: crewai decode task: %w", err)
	}
	return &t, nil
}

// crewDecodeTaskBody is crewDecodeTaskResponse over an already-200 response,
// additionally returning the raw body so callers can re-inspect error
// envelopes (the unsupported-streaming fallback).
func crewDecodeTaskBody(resp *http.Response) (*crewWireTask, []byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	closeErr := resp.Body.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("mesh: crewai read response: %w", err)
	}
	if closeErr != nil {
		return nil, nil, fmt.Errorf("mesh: crewai close response: %w", closeErr)
	}
	var env crewWireEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, body, fmt.Errorf("mesh: crewai decode response: %w", err)
	}
	if env.Error != nil {
		return nil, body, fmt.Errorf("mesh: crewai JSON-RPC error %d: %s", env.Error.Code, env.Error.Message)
	}
	if env.Result == nil {
		return nil, body, fmt.Errorf("mesh: crewai response carried no result")
	}
	var t crewWireTask
	if err := json.Unmarshal(env.Result, &t); err != nil {
		return nil, body, fmt.Errorf("mesh: crewai decode task: %w", err)
	}
	return &t, body, nil
}

// --- wire types (Google A2A 0.3.x subset the router speaks) --------------

// crewWireRequest is the JSON-RPC 2.0 envelope.
type crewWireRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// crewWireEnvelope is the JSON-RPC response envelope; Result stays raw json so
// the classifier can discriminate 0.3.x kind frames and 1.0 wrapper frames.
type crewWireEnvelope struct {
	Result json.RawMessage   `json:"result,omitempty"`
	Error  *crewWireRPCError `json:"error,omitempty"`
	ID     int               `json:"id"`
}

type crewWireRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// crewWireSendParams is the 0.3.x message/send / message/stream params.
type crewWireSendParams struct {
	Message crewWireMessage `json:"message"`
}

// crewWireMessage is the 0.3.x Message (role/parts carry the 0.3.x kind
// discriminators; metadata rides the correlation ids).
type crewWireMessage struct {
	Role      string            `json:"role"`
	Parts     []crewWirePart    `json:"parts"`
	MessageID string            `json:"messageId,omitempty"`
	TaskID    string            `json:"taskId,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type crewWirePart struct {
	Kind string `json:"kind,omitempty"` // 0.3.x discriminator; absent at 1.0
	Text string `json:"text,omitempty"`
}

// crewWireTask is the crew-side Task snapshot (0.3.x shape; unknown fields
// ignored so a 1.0 server parses too).
type crewWireTask struct {
	Kind      string         `json:"kind,omitempty"`
	ID        string         `json:"id"`
	ContextID string         `json:"contextId"`
	Status    crewWireStatus `json:"status"`
	Artifacts []crewWireArte `json:"artifacts,omitempty"`
}

type crewWireStatus struct {
	State   string           `json:"state"`
	Message *crewWireMessage `json:"message,omitempty"`
}

type crewWireArte struct {
	ArtifactID string         `json:"artifactId"`
	Name       string         `json:"name,omitempty"`
	Parts      []crewWirePart `json:"parts,omitempty"`
	LastChunk  bool           `json:"lastChunk,omitempty"`
}

// crewWireTaskIDParams is the tasks/get / tasks/cancel params.
type crewWireTaskIDParams struct {
	ID string `json:"id"`
}

// crewFrameKind classifies one SSE result payload.
type crewFrameKind int

const (
	crewFrameTask crewFrameKind = iota
	crewFrameStatusUpdate
	crewFrameArtifactUpdate
	crewFrameMessage
)

// crewFrame is one decoded SSE result: its kind plus the decoded halves. A
// frame carries exactly one of Task / Status / Artifact / Message.
type crewFrame struct {
	kind     crewFrameKind
	task     crewWireTask
	status   crewWireStatus
	artifact crewWireArte
	message  crewWireMessage
	// final is the 0.3.x TaskStatusUpdateEvent.final flag (removed at 1.0,
	// where stream close alone signals termination).
	final bool
}

// crewClassify decodes one SSE data payload's result object, tolerating both
// the 0.3.x kind discriminator ("kind": "status-update", …) and the 1.0
// StreamResponse wrapper-member shapes ({"statusUpdate": {...}}).
func crewClassify(result json.RawMessage) (*crewFrame, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(result, &probe); err != nil {
		return nil, fmt.Errorf("mesh: crewai frame is not an object: %w", err)
	}
	// 1.0 wrapper: unwrap the oneof member and classify the inner object by
	// shape (the kind discriminator is gone at 1.0).
	for _, wrap := range []string{"task", "statusUpdate", "artifactUpdate", "message"} {
		if raw, ok := probe[wrap]; ok {
			result = raw
			if err := json.Unmarshal(result, &probe); err != nil {
				return nil, fmt.Errorf("mesh: crewai decode %s frame: %w", wrap, err)
			}
			return crewClassifyShape(probe, result)
		}
	}
	// 0.3.x explicit discriminator.
	switch string2kind(probe["kind"]) {
	case "task":
		var f crewFrame
		f.kind = crewFrameTask
		return &f, json.Unmarshal(result, &f.task)
	case "status-update":
		return decodeStatusUpdate(result)
	case "artifact-update":
		return decodeArtifactUpdate(result)
	case "message":
		var f crewFrame
		f.kind = crewFrameMessage
		return &f, json.Unmarshal(result, &f.message)
	}
	return crewClassifyShape(probe, result)
}

// crewClassifyShape classifies a discriminator-less frame by its members:
// artifact-update carries artifact, status-update carries taskId+status, a
// message carries role, a Task snapshot carries id+status.
func crewClassifyShape(probe map[string]json.RawMessage, result json.RawMessage) (*crewFrame, error) {
	_, hasArt := probe["artifact"]
	_, hasTaskID := probe["taskId"]
	_, hasStatus := probe["status"]
	_, hasRole := probe["role"]
	_, hasID := probe["id"]
	switch {
	case hasArt:
		return decodeArtifactUpdate(result)
	case hasTaskID && hasStatus:
		return decodeStatusUpdate(result)
	case hasRole:
		var f crewFrame
		f.kind = crewFrameMessage
		return &f, json.Unmarshal(result, &f.message)
	case hasID && hasStatus:
		var f crewFrame
		f.kind = crewFrameTask
		return &f, json.Unmarshal(result, &f.task)
	}
	return nil, fmt.Errorf("mesh: crewai frame carries no recognizable task surface")
}

func decodeStatusUpdate(result json.RawMessage) (*crewFrame, error) {
	var su struct {
		TaskID string         `json:"taskId"`
		Status crewWireStatus `json:"status"`
		Final  bool           `json:"final"`
	}
	if err := json.Unmarshal(result, &su); err != nil {
		return nil, err
	}
	return &crewFrame{kind: crewFrameStatusUpdate, status: su.Status, final: su.Final}, nil
}

// decodeArtifactUpdate lifts the artifact nested under the frame's "artifact"
// member (both the 0.3.x kind frame and the 1.0 wrapper unwrap to this shape).
func decodeArtifactUpdate(result json.RawMessage) (*crewFrame, error) {
	var au struct {
		Artifact crewWireArte `json:"artifact"`
	}
	if err := json.Unmarshal(result, &au); err != nil {
		return nil, err
	}
	return &crewFrame{kind: crewFrameArtifactUpdate, artifact: au.Artifact}, nil
}

func string2kind(raw json.RawMessage) string {
	if len(raw) < 2 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// --- state & event mapping (module a2a.Event ← Google A2A frames) --------

// crewMapState maps a Google TaskState onto the module §3.1 state machine.
// The 0.3.x slugs (submitted, working, input-required, completed, failed,
// canceled, auth-required) map 1:1; the 1.0 SCREAMING forms and the
// task-state/ prefix normalize to the same values. rejected (module has no
// such state) maps to failed with its name preserved in the reason;
// unknown/unspecified maps to submitted (the pre-drive state) rather than
// fabricating a failure.
func crewMapState(raw string) (a2a.TaskState, string) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, "task-state/")
	s = strings.TrimPrefix(s, "task_state_")
	// Proto-JSON multi-word forms (TASK_STATE_INPUT_REQUIRED) carry
	// underscores where the slug forms carry dashes.
	s = strings.ReplaceAll(s, "_", "-")
	switch s {
	case "", "unspecified", "unknown", "submitted":
		return a2a.TaskSubmitted, ""
	case "working":
		return a2a.TaskWorking, ""
	case "input-required":
		return a2a.TaskInputRequired, ""
	case "auth-required":
		return a2a.TaskAuthRequired, ""
	case "completed":
		return a2a.TaskCompleted, ""
	case "failed":
		return a2a.TaskFailed, ""
	case "canceled", "cancelled":
		return a2a.TaskCanceled, ""
	case "rejected":
		return a2a.TaskFailed, "crew rejected the task"
	default:
		return a2a.TaskWorking, "unmapped crew state: " + raw
	}
}

// crewTaskText composes the message text from the module task's envelope: the
// system context rides ahead of the concrete input, exactly as the shim
// engines drive their runtimes (spec §3 V1, §8.5).
func crewTaskText(t a2a.Task) string {
	switch {
	case t.Envelope.SystemContext != "" && t.Envelope.Input != "":
		return t.Envelope.SystemContext + "\n\n" + t.Envelope.Input
	case t.Envelope.Input != "":
		return t.Envelope.Input
	default:
		return t.Envelope.SystemContext
	}
}

// crewTaskMetadata rides the module correlation ids on the Google message so a
// crew-side trace of the task can be tied back to its Run (the ids are also
// the message id, but metadata keeps them addressable server-side).
func crewTaskMetadata(t a2a.Task) map[string]string {
	md := map[string]string{"a2a_task_id": t.A2ATaskID}
	if t.WorkItemID != "" {
		md["work_item_id"] = t.WorkItemID
	}
	if t.FenceToken != "" {
		md["fence_token"] = t.FenceToken
	}
	return md
}

// crewPartsText joins a frame's text parts into one display string. Non-text
// parts (data/file) have no module event surface yet; their presence is not
// fabricated, only the text they carry is surfaced.
func crewPartsText(parts []crewWirePart) string {
	var sb strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// crewMessageEvent builds an EventMessage from agent-side text (F16: untrusted,
// display only).
func crewMessageEvent(taskID string, seq uint64, role, text string) a2a.Event {
	if role == "" {
		role = "agent"
	}
	return a2a.Event{
		Seq:       seq,
		A2ATaskID: taskID,
		TS:        time.Now().UTC(),
		Type:      a2a.EventMessage,
		Payload: a2a.MessagePayload{
			Role:  role,
			Text:  text,
			Trust: "untrusted",
		},
	}
}

// crewStatusEvent builds an EventStatus frame.
func crewStatusEvent(taskID string, seq uint64, state a2a.TaskState, reason string) a2a.Event {
	return a2a.Event{
		Seq:       seq,
		A2ATaskID: taskID,
		TS:        time.Now().UTC(),
		Type:      a2a.EventStatus,
		Payload: a2a.StatusPayload{
			State:  state,
			Reason: reason,
		},
	}
}

// crewArtifactRefEvent builds the EventArtifactRef for one crew artifact.
// Google artifacts are inline content, not coord-committed URIs, so the ref
// points at the crew-side artifact address (a2a://<task>/<artifactId>) with an
// empty sha256 — an unverifiable ref is left empty, never fabricated; the
// content itself rides an accompanying EventMessage so nothing is lost.
func crewArtifactRefEvent(taskID string, seq uint64, googleTaskID string, art crewWireArte) a2a.Event {
	return a2a.Event{
		Seq:       seq,
		A2ATaskID: taskID,
		TS:        time.Now().UTC(),
		Type:      a2a.EventArtifactRef,
		Payload: a2a.ArtifactRef{
			Kind:   "google-a2a-artifact",
			URI:    fmt.Sprintf("a2a://%s/%s", googleTaskID, art.ArtifactID),
			SHA256: "",
		},
	}
}

// --- crewStream: the Router's Stream over SSE or poll reattach ------------

// crewStream tails one crew task as a module Stream. It starts either over
// the message/stream SSE body (pumpSSE) or over tasks/get polling
// (pumpPoll — the non-streaming fallback and the C1 reattach path); a torn
// SSE body on a still-live task hands over to polling so the follower keeps a
// gap-free sequence to the terminal state.
type crewStream struct {
	router   *crewaiRouter
	endpoint string
	taskID   string // module a2a_task_id
	st       *crewTaskState

	body   io.ReadCloser // non-nil on the SSE path
	events chan a2a.Event
	quit   chan struct{}

	closeOnce sync.Once

	mu      sync.Mutex
	closed  bool
	settled bool
}

// Events is the task's sequenced event stream; closed exactly once.
func (s *crewStream) Events() <-chan a2a.Event { return s.events }

// Status is the current status; after Events() closes it is terminal (or the
// last observed state on a torn tail).
func (s *crewStream) Status() a2a.Status { return s.st.currentStatus() }

// Close stops the pump and tears the transport down. If the task has not
// settled, it ALSO issues the V4 mapping — a best-effort tasks/cancel —
// mirroring the HTTP router's contract where Close aborting the in-flight
// request cancels the server-side run. Idempotent.
func (s *crewStream) Close() error {
	s.mu.Lock()
	s.closed = true
	settled := s.settled
	s.mu.Unlock()
	googleID, terminal := s.st.snapshot()
	shouldCancel := !settled && !terminal && googleID != ""
	s.closeOnce.Do(func() {
		close(s.quit)
		if s.body != nil {
			_ = s.body.Close()
		}
		if shouldCancel {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.router.crewCancelTask(ctx, s.endpoint, googleID)
		}
	})
	return nil
}

// pumpSSE tails the message/stream response: data frames carry JSON-RPC
// envelopes whose result is one crew frame. On body end with the task still
// live, it hands over to poll reattach instead of settling short.
func (s *crewStream) pumpSSE() {
	defer s.finish()
	sc := bufio.NewScanner(s.body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for {
		select {
		case <-s.quit:
			return
		default:
		}
		if !sc.Scan() {
			// Body ended (terminal close or a tear).
			s.mu.Lock()
			settled := s.settled
			s.mu.Unlock()
			googleID, terminal := s.st.snapshot()
			switch {
			case settled:
				return
			case terminal:
				return
			case googleID == "":
				// The stream died before any task frame arrived — there is
				// nothing to poll, so fail the task honestly instead of
				// tailing forever.
				s.applyStatus(crewWireStatus{State: "failed", Message: &crewWireMessage{
					Role:  "agent",
					Parts: []crewWirePart{{Kind: "text", Text: "crewai: event stream ended before a task was created"}},
				}}, true)
				return
			default:
				// Live task, torn tail: reattach by polling.
				s.pumpPoll(context.Background())
				return
			}
		}
		line := sc.Text()
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue // keep-alive comments, event: lines, blank separators
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var env crewWireEnvelope
		if err := json.Unmarshal([]byte(payload), &env); err != nil {
			continue // malformed frame: never kill a live tail over one bad line
		}
		if env.Result == nil {
			continue // mid-stream JSON-RPC error envelope; the tail continues
		}
		frame, err := crewClassify(env.Result)
		if err != nil {
			continue
		}
		if stop := s.apply(frame); stop {
			return
		}
	}
}

// pumpPoll tails the task with tasks/get until terminal, emitting a status
// event per observed state change and artifact events for artifacts not yet
// reported. This is the C1 reattach path and the non-streaming fallback.
func (s *crewStream) pumpPoll(ctx context.Context) {
	defer s.finish()
	tick := time.NewTicker(crewaiReattachPoll)
	defer tick.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		googleID, _ := s.st.snapshot()
		if googleID == "" {
			continue // submit not yet acknowledged; nothing to poll
		}
		gt, err := s.router.crewGetTask(ctx, s.endpoint, googleID)
		if err != nil {
			continue // transient poll failure: keep the tail alive
		}
		if stop := s.applyTask(gt); stop {
			return
		}
	}
}

// finish settles the stream: the status is whatever st last recorded; the
// task graduates to the completed map when terminal (bounded).
func (s *crewStream) finish() {
	s.mu.Lock()
	if s.settled {
		s.mu.Unlock()
		return
	}
	s.settled = true
	s.mu.Unlock()
	if _, terminal := s.st.snapshot(); terminal {
		s.router.settle(s.taskID, s.st)
	}
	close(s.events)
}

// apply converts one crew frame, emits the module events, and reports whether
// the task reached a terminal state (the pump's stop signal).
func (s *crewStream) apply(f *crewFrame) bool {
	switch f.kind {
	case crewFrameTask:
		// Snapshot: adopt ids, surface any artifacts already present, then
		// treat the embedded status as one observation.
		s.st.adoptIDs(f.task.ID, f.task.ContextID)
		for _, art := range f.task.Artifacts {
			s.emitArtifact(art)
		}
		return s.applyStatus(f.task.Status, false)
	case crewFrameStatusUpdate:
		return s.applyStatus(f.status, f.final)
	case crewFrameArtifactUpdate:
		s.emitArtifact(f.artifact)
		return false
	case crewFrameMessage:
		if text := crewPartsText(f.message.Parts); text != "" {
			s.send(crewMessageEvent(s.taskID, s.st.nextSeq(), f.message.Role, text))
		}
		return false
	}
	return false
}

// applyStatus maps one status observation, emits the status event, and reports
// terminal. The 0.3.x final flag settles even when the state name is not one
// of the module terminal states (defense against an unknown-terminal state).
func (s *crewStream) applyStatus(ws crewWireStatus, final bool) bool {
	state, reason := crewMapState(ws.State)
	if ws.Message != nil {
		if text := crewPartsText(ws.Message.Parts); text != "" {
			if reason == "" {
				reason = a2a.ToolErrorSummary(text)
			}
			s.send(crewMessageEvent(s.taskID, s.st.nextSeq(), ws.Message.Role, text))
		}
	}
	sp := a2a.StatusPayload{State: state, Reason: reason}
	seq := s.st.nextSeq()
	s.send(a2a.Event{
		Seq:       seq,
		A2ATaskID: s.taskID,
		TS:        time.Now().UTC(),
		Type:      a2a.EventStatus,
		Payload:   sp,
	})
	return s.st.settleStatus(a2a.Status{State: sp.State, Reason: sp.Reason, LastSeq: seq}, state.IsTerminal() || final)
}

// emitArtifact reports one artifact update (ref + text) unless already seen.
func (s *crewStream) emitArtifact(art crewWireArte) {
	if !s.st.claimArtifact(art.ArtifactID) {
		return
	}
	googleID, _ := s.st.snapshot()
	ref := crewArtifactRefEvent(s.taskID, s.st.nextSeq(), googleID, art)
	s.send(ref)
	if text := crewPartsText(art.Parts); text != "" {
		s.send(crewMessageEvent(s.taskID, s.st.nextSeq(), "agent", text))
	}
}

// applyTask maps a polled Task snapshot: new artifacts first, then status.
func (s *crewStream) applyTask(gt *crewWireTask) bool {
	for _, art := range gt.Artifacts {
		s.emitArtifact(art)
	}
	return s.applyStatus(gt.Status, false)
}

// send forwards one event unless the follower has dropped.
func (s *crewStream) send(ev a2a.Event) bool {
	select {
	case s.events <- ev:
		return true
	case <-s.quit:
		return false
	}
}

// settle moves a terminal task from the in-flight map into the bounded
// completed map (a later re-route replays the terminal status).
func (r *crewaiRouter) settle(taskID string, st *crewTaskState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight != nil && r.inflight[taskID] == st {
		delete(r.inflight, taskID)
	}
	if r.completed == nil {
		r.completed = map[string]*crewTaskState{}
	}
	r.completed[taskID] = st
	if len(r.completed) > crewaiCompletedTrack {
		// Drop an arbitrary entry: the bound is a leak guard, not an LRU
		// contract — a re-route past the bound degrades to a fresh submit.
		for k := range r.completed {
			delete(r.completed, k)
			if len(r.completed) <= crewaiCompletedTrack {
				break
			}
		}
	}
}

// --- replay stream (C1: re-route on a terminal task) ----------------------

// newCrewReplayStream answers a re-route on a task the router already saw
// terminal: it replays the recorded terminal status event once and closes —
// no crew round-trip, no re-send (C1's "a submit on a terminal task returns
// the terminal status").
func newCrewReplayStream(taskID string, st *crewTaskState) Stream {
	status := st.currentStatus()
	st.mu.Lock()
	seq := st.lastSeq + 1
	st.mu.Unlock()
	events := make(chan a2a.Event, 1)
	events <- crewStatusEvent(taskID, seq, status.State, status.Reason)
	close(events)
	return &crewReplayStream{events: events, status: status}
}

type crewReplayStream struct {
	events chan a2a.Event
	status a2a.Status
}

func (s *crewReplayStream) Events() <-chan a2a.Event { return s.events }
func (s *crewReplayStream) Status() a2a.Status       { return s.status }
func (s *crewReplayStream) Close() error             { return nil }

var _ Stream = (*crewStream)(nil)
var _ Stream = (*crewReplayStream)(nil)
