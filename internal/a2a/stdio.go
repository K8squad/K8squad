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
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"

	wire "github.com/K8squad/K8squad/pkg/a2a"
)

// StdioTransport is the production v1 A2A transport: it launches the runtime
// shim as a subprocess (`shim run`, cmd/shim) and speaks the stdio-framed wire —
// one Task JSON on stdin, a gap-free JSONL SSE stream on stdout, exit code
// reflecting the terminal outcome (cmd/shim.driveRun). It carries only the A2A
// Task; the reconciler env-injects the credential Secret (§7.3) into the
// command's environment out of band, so the transport never reads the raw
// secret.
type StdioTransport struct {
	// Command builds the exec.Cmd for one task run. It MUST select the `run`
	// subcommand and set the shim's environment (KSQUAD_RUNTIME_TYPE, the
	// mounted credential, §7.2/§7.3); the transport wires stdin/stdout. It is
	// called once per Submit so a re-drive gets a fresh process (the shim's own
	// idempotency — C1 — is enforced by the task id, not by process identity).
	Command func(ctx context.Context, t wire.Task) (*exec.Cmd, error)
	// Stderr, if set, receives the shim's diagnostic stream. It is NEVER the SSE
	// channel and MUST NOT be parsed as events.
	Stderr io.Writer
}

// Submit launches the shim, feeds it the single task on stdin, and returns a
// Session tailing its JSONL SSE stream.
func (t *StdioTransport) Submit(ctx context.Context, task wire.Task) (Session, error) {
	if t.Command == nil {
		return nil, fmt.Errorf("a2a: StdioTransport requires a Command builder")
	}
	if task.A2ATaskID == "" {
		return nil, fmt.Errorf("a2a: StdioTransport.Submit requires a non-empty a2a_task_id")
	}
	cmd, err := t.Command(ctx, task)
	if err != nil {
		return nil, fmt.Errorf("a2a: build shim command: %w", err)
	}
	payload, err := json.Marshal(task)
	if err != nil {
		return nil, fmt.Errorf("a2a: marshal task %s: %w", task.A2ATaskID, err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("a2a: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("a2a: stdout pipe: %w", err)
	}
	if t.Stderr != nil {
		cmd.Stderr = t.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("a2a: start shim: %w", err)
	}

	// Feed the one task, then close stdin so the shim's decoder returns and it
	// begins driving. A write error surfaces on the process exit, not here.
	go func() {
		defer stdin.Close()
		_, _ = stdin.Write(payload)
	}()

	sess := &stdioSession{
		taskID: task.A2ATaskID,
		cmd:    cmd,
		events: make(chan wire.Event),
		quit:   make(chan struct{}),
	}
	go sess.pump(stdout)
	return sess, nil
}

// stdioSession tails a shim subprocess's JSONL SSE stream.
type stdioSession struct {
	taskID string
	cmd    *exec.Cmd
	events chan wire.Event
	quit   chan struct{}

	closeOnce sync.Once

	mu      sync.Mutex
	status  wire.Status
	settled bool
}

// pump decodes JSONL events from the shim's stdout, forwards them, and settles
// the session's status from the last status event + process exit when the
// stream ends. It always closes the events channel exactly once.
func (s *stdioSession) pump(r io.Reader) {
	defer close(s.events)
	dec := json.NewDecoder(r)
	var last wire.Status
	for {
		var ev wire.Event
		if err := dec.Decode(&ev); err != nil {
			break // EOF or a malformed tail: the stream is done.
		}
		last.LastSeq = ev.Seq
		if ev.Type == wire.EventStatus {
			if sp, ok := statusPayload(ev.Payload); ok {
				last.State = sp.State
				last.Reason = sp.Reason
			}
		}
		select {
		case s.events <- ev:
		case <-s.quit:
			// The follower dropped us (ctx canceled / sink error). Stop pumping
			// and let Close tear the process down.
			_ = s.cmd.Wait()
			return
		}
	}
	// Reap the process so we do not leak a zombie; its exit code corroborates a
	// failed terminal state but the status events are authoritative.
	_ = s.cmd.Wait()
	s.mu.Lock()
	s.status = last
	s.settled = true
	s.mu.Unlock()
}

func (s *stdioSession) Events() <-chan wire.Event { return s.events }

func (s *stdioSession) Status() wire.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Cancel tears the shim process down. There is no separate cancel RPC on the
// stdio wire (the process IS the task), so killing it drains the task; it is an
// idempotent no-op once the process has exited (C8).
func (s *stdioSession) Cancel(ctx context.Context, reason string) error {
	return s.Close()
}

// Close signals the pump to stop and kills the shim process. Idempotent.
func (s *stdioSession) Close() error {
	s.closeOnce.Do(func() {
		close(s.quit)
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	})
	return nil
}
