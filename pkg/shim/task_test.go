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

package shim

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/a2a"
)

// TestEmitHoldsTaskLockAcrossAppend is the ISI-3184 F1 regression. The bug: seq
// was assigned under t.mu but the stream append happened after unlocking, so a
// progress emit racing a terminate could append its lower-seq event *after* the
// higher-seq terminal event — replaying seq out of order (C4 violation) and
// dropping the straggler on resume. The narrow window makes it near-impossible
// to hit probabilistically (why -race + the concurrency test below miss it), so
// this asserts the invariant that closes it deterministically: emitProgress
// must still hold t.mu while it appends to the stream.
//
// Mechanism: hold the stream mutex so any append blocks, launch emitProgress,
// and assert t.mu cannot be acquired. Fixed code parks inside append still
// holding t.mu → TryLock fails. Buggy code released t.mu before appending →
// TryLock succeeds and the test fails.
func TestEmitHoldsTaskLockAcrossAppend(t *testing.T) {
	tk := &task{id: "run-x", state: a2a.TaskWorking, stream: newTaskStream(), now: time.Now}
	tk.setState(a2a.TaskWorking, "")

	// Block every stream append by holding the stream's mutex directly.
	tk.stream.mu.Lock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		tk.emitProgress(msg("mid-flight")) // blocks inside stream.append
	}()

	// Let the emit goroutine acquire t.mu and park on the blocked append.
	time.Sleep(100 * time.Millisecond)

	if tk.mu.TryLock() {
		tk.mu.Unlock()
		tk.stream.mu.Unlock()
		<-done
		t.Fatal("emitProgress released t.mu before appending: seq assignment and stream append are not atomic (C4 F1 regression)")
	}

	// Release the stream; emitProgress finishes its append and returns.
	tk.stream.mu.Unlock()
	<-done
}

// TestEmitTerminateOrdering exercises the concurrent scenario the reviewer
// called out — a progress emit racing a CancelTask terminate — and asserts the
// drained SSE log is strictly increasing in seq (gap-free monotonic, C4). It
// runs under -race for good measure; the deterministic guarantee is proven by
// TestEmitHoldsTaskLockAcrossAppend above.
func TestEmitTerminateOrdering(t *testing.T) {
	for i := 0; i < 500; i++ {
		tk := &task{id: "run-race", state: a2a.TaskWorking, stream: newTaskStream(), now: time.Now}
		tk.setState(a2a.TaskWorking, "")

		ch := tk.stream.subscribe(context.Background(), 0)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); tk.emitProgress(msg("mid-flight")) }()
		go func() { defer wg.Done(); tk.terminate(a2a.TaskCanceled, "canceled") }()
		wg.Wait()

		var last uint64
		for ev := range ch {
			if ev.Seq <= last {
				t.Fatalf("iter %d: seq %d appended after seq %d — non-monotonic log (C4 violation)", i, ev.Seq, last)
			}
			last = ev.Seq
		}
	}
}
