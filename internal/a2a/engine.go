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

package a2a

import (
	"context"

	wire "github.com/K8squad/K8squad/pkg/a2a"
)

// EngineTransport drives an in-process wire.Shim (e.g. pkg/shim.Engine) as the
// A2A transport. It is the single-binary / dev / conformance path where the
// runtime shim runs in the same process as the core — no subprocess, no socket.
// The production multi-image path is StdioTransport. Because it speaks the same
// wire.Shim verbs, a client written against EngineTransport is byte-for-byte the
// same as one against a remote transport (the point of the seam, R11).
type EngineTransport struct {
	// Shim is the in-process runtime shim (the southbound A2A contract, §3).
	Shim wire.Shim
}

// NewEngineTransport returns an EngineTransport over sh.
func NewEngineTransport(sh wire.Shim) *EngineTransport { return &EngineTransport{Shim: sh} }

// Submit submits t to the in-process shim and subscribes to its event stream
// from seq 0 so the caller sees the full lifecycle including the seeded
// submitted state (C4). SubmitTask is idempotent on the task id (C1), so a
// re-drive reattaches to the in-flight task.
func (t *EngineTransport) Submit(ctx context.Context, task wire.Task) (Session, error) {
	if _, err := t.Shim.SubmitTask(ctx, task); err != nil {
		return nil, err
	}
	ch, err := t.Shim.StreamEvents(ctx, task.A2ATaskID, 0)
	if err != nil {
		return nil, err
	}
	return &engineSession{shim: t.Shim, taskID: task.A2ATaskID, events: ch}, nil
}

// engineSession is a Session backed by an in-process wire.Shim.
type engineSession struct {
	shim   wire.Shim
	taskID string
	events <-chan wire.Event
}

func (s *engineSession) Events() <-chan wire.Event { return s.events }

// Status reads the shim's current status for the task; after the stream closes
// this is the terminal status.
func (s *engineSession) Status() wire.Status {
	st, err := s.shim.GetStatus(context.Background(), s.taskID)
	if err != nil {
		return wire.Status{}
	}
	return st
}

// Cancel drains the task to canceled via the shim (idempotent, C8).
func (s *engineSession) Cancel(ctx context.Context, reason string) error {
	return s.shim.CancelTask(ctx, s.taskID, reason)
}

// Close is a no-op: an in-process shim owns no per-session OS resource.
func (s *engineSession) Close() error { return nil }
