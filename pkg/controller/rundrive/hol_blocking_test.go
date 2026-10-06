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

package rundrive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/K8squad/K8squad/internal/a2a"
	wire "github.com/K8squad/K8squad/pkg/a2a"
)

// ISI-5524: the run-drive controller ran with controller-runtime's implicit
// MaxConcurrentReconciles=1, so a single Run whose dispatch blocked (a team
// whose model endpoint stalled the POST to the sandbox supervisor) starved
// every other team's Run. workers() is the configurable floor.
func TestDriverWorkersDefaultsAndOverride(t *testing.T) {
	cases := []struct {
		name string
		set  int
		want int
	}{
		{"unset defaults", 0, DefaultWorkers},
		{"negative defaults", -3, DefaultWorkers},
		{"override honored", 8, 8},
		{"single still allowed", 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Driver{Workers: tc.set}
			if got := d.workers(); got != tc.want {
				t.Fatalf("workers() = %d, want %d", got, tc.want)
			}
		})
	}
}

// newDispatchHTTPClient must set ResponseHeaderTimeout (the header-only bound,
// never a whole-request timeout — the event body streams unbounded) and fall
// back to the default when the config leaves it unset.
func TestNewDispatchHTTPClientHeaderTimeout(t *testing.T) {
	t.Run("explicit", func(t *testing.T) {
		c := newDispatchHTTPClient(5 * time.Second)
		if c.Timeout != 0 {
			t.Fatalf("Client.Timeout = %s, want 0 (body must stream unbounded)", c.Timeout)
		}
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
		}
		if tr.ResponseHeaderTimeout != 5*time.Second {
			t.Fatalf("ResponseHeaderTimeout = %s, want 5s", tr.ResponseHeaderTimeout)
		}
	})
	t.Run("default", func(t *testing.T) {
		c := newDispatchHTTPClient(0)
		tr := c.Transport.(*http.Transport)
		if tr.ResponseHeaderTimeout != defaultDispatchHeaderTimeout {
			t.Fatalf("ResponseHeaderTimeout = %s, want default %s", tr.ResponseHeaderTimeout, defaultDispatchHeaderTimeout)
		}
	})
}

// The end-to-end proof of the acceptance criterion "no ~3m in-handler blocking
// holds the run-drive workqueue": a supervisor that stalls before flushing its
// response headers (the shape a wedged model endpoint produces, since the
// supervisor writes 200 only after it has accepted the task) must NOT block the
// POST for the stall's full duration — the bounded client returns an error
// promptly, which the driver turns into a requeue that frees the worker.
func TestDispatchHeaderTimeoutFreesBlockedPOST(t *testing.T) {
	const headerBound = 150 * time.Millisecond

	// A supervisor stuck for far longer than the header bound before it would
	// ever write a status line.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ht := &a2a.HTTPTransport{
		URL:    func(context.Context, wire.Task) (string, error) { return srv.URL + "/task", nil },
		Client: newDispatchHTTPClient(headerBound),
	}

	start := time.Now()
	_, err := ht.Submit(context.Background(), wire.Task{A2ATaskID: "run-wedged#lap1"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Submit returned nil error against a stalled supervisor; expected a header-timeout error")
	}
	// Must give up near the header bound, nowhere near the 5s stall (let alone
	// the minutes the unbounded client allowed).
	if elapsed > time.Second {
		t.Fatalf("Submit blocked %s; the bounded client should return within ~%s", elapsed, headerBound)
	}
}
