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
	"context"
	"fmt"
	"testing"
)

// Test battery re-homed from the reference host's route-leg tests (rundrive
// meshroute tests, ISI-5822 P1.1): the decision mechanism, the canary set
// parsing and the saturation match lift with the code; the Run-CR stamping
// tests stay host-side (they exercise the host's run-object bindings).

// fakeLookup is the EndpointLookup seam bound in tests.
type fakeLookup struct {
	eps []Endpoint
	err error
}

func (f fakeLookup) LookupTeamRole(context.Context, string, string) ([]Endpoint, error) {
	return f.eps, f.err
}

// Route stays dark with no config, an unflagged team, or an empty registry
// (the reversibility fallback); routes to the first LRU endpoint for a
// flagged team; errors loudly on a registry failure.
func TestRoute_Decisions(t *testing.T) {
	eps := []Endpoint{{EndpointID: "e0", Endpoint: "http://10.0.0.1:8081"}}
	team, role := "team-acme", "coder"

	t.Run("nil config", func(t *testing.T) {
		if _, use, err := Route(context.Background(), nil, team, role); use || err != nil {
			t.Fatalf("use=%v err=%v, want legacy no-op", use, err)
		}
	})
	t.Run("nil members", func(t *testing.T) {
		cfg := &RouteConfig{Teams: TeamSet{team: {}}}
		if _, use, err := Route(context.Background(), cfg, team, role); use || err != nil {
			t.Fatalf("use=%v err=%v, want legacy no-op", use, err)
		}
	})
	t.Run("unflagged team", func(t *testing.T) {
		cfg := &RouteConfig{Teams: TeamSet{"other": {}}, Lookup: fakeLookup{eps: eps}}
		if _, use, err := Route(context.Background(), cfg, team, role); use || err != nil {
			t.Fatalf("use=%v err=%v, want legacy no-op", use, err)
		}
	})
	t.Run("identity-less dispatch", func(t *testing.T) {
		cfg := &RouteConfig{Teams: TeamSet{team: {}}, Lookup: fakeLookup{eps: eps}}
		if _, use, err := Route(context.Background(), cfg, "", role); use || err != nil {
			t.Fatalf("use=%v err=%v, want legacy no-op", use, err)
		}
	})
	t.Run("empty registry falls back to legacy", func(t *testing.T) {
		cfg := &RouteConfig{Teams: TeamSet{team: {}}, Lookup: fakeLookup{}}
		if _, use, err := Route(context.Background(), cfg, team, role); use || err != nil {
			t.Fatalf("use=%v err=%v, want legacy fallback", use, err)
		}
	})
	t.Run("flagged team routes to first endpoint", func(t *testing.T) {
		cfg := &RouteConfig{Teams: TeamSet{team: {}}, Lookup: fakeLookup{eps: eps}}
		url, use, err := Route(context.Background(), cfg, team, role)
		if err != nil || !use {
			t.Fatalf("use=%v err=%v, want mesh route", use, err)
		}
		if url != "http://10.0.0.1:8081/task" {
			t.Fatalf("url = %q, want the endpoint /task surface", url)
		}
	})
	t.Run("endpoint trailing slash is trimmed", func(t *testing.T) {
		cfg := &RouteConfig{Teams: TeamSet{team: {}}, Lookup: fakeLookup{
			eps: []Endpoint{{EndpointID: "e0", Endpoint: "http://10.0.0.1:8081/"}},
		}}
		url, use, err := Route(context.Background(), cfg, team, role)
		if err != nil || !use {
			t.Fatalf("use=%v err=%v, want mesh route", use, err)
		}
		if url != "http://10.0.0.1:8081/task" {
			t.Fatalf("url = %q, want the trailing slash trimmed", url)
		}
	})
	t.Run("registry error is loud", func(t *testing.T) {
		cfg := &RouteConfig{Teams: TeamSet{team: {}}, Lookup: fakeLookup{err: context.Canceled}}
		if _, _, err := Route(context.Background(), cfg, team, role); err == nil {
			t.Fatal("want error on lookup failure")
		}
	})
}

// TeamSet parses the per-team canary list (D9.3): whitespace-tolerant,
// empties dropped; an unset list yields the empty set (flag off
// cluster-wide).
func TestTeamSet_Parsing(t *testing.T) {
	s := NewTeamSet(" team-acme , , other ")
	if !s.Enabled("team-acme") || !s.Enabled("other") || s.Enabled("team-b") {
		t.Fatalf("set = %v", s)
	}
	if len(NewTeamSet("")) != 0 {
		t.Fatal("unset list must yield an empty set (flag off cluster-wide)")
	}
}

// IsSaturation recognizes the gateway's saturation refusal — 409 plus one
// of the gateway-local sentinel texts (D3.3 guard #3) — and nothing else.
// Matched on "409 Conflict", not bare "409": a port number in the folded
// URL must never read as the status.
func TestIsSaturation(t *testing.T) {
	sentinel := fmt.Sprintf("a2a: POST http://10.0.0.1:4090/task: supervisor answered 409 Conflict: %v", ErrProcessBusy)
	if !IsSaturation(fmt.Errorf("%s", sentinel)) {
		t.Fatal("409 + busy sentinel must read as saturation")
	}
	if !IsSaturation(fmt.Errorf("a2a: POST /task: supervisor answered 409 Conflict: %v", ErrNoIdleProcess)) {
		t.Fatal("409 + no-idle sentinel must read as saturation")
	}
	if IsSaturation(fmt.Errorf("a2a: POST http://10.0.0.1:4090/task: supervisor answered 502 Bad Gateway: boom")) {
		t.Fatal("a 502 with a 409x port must not read as saturation")
	}
	if IsSaturation(fmt.Errorf("a2a: POST /task: supervisor answered 409 Conflict: boom")) {
		t.Fatal("409 without a sentinel must not read as saturation")
	}
	if IsSaturation(nil) {
		t.Fatal("nil error is never saturation")
	}
}
