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

package v1alpha1

import (
	"encoding/json"
	"testing"
)

// D-AC2: the export reconciler seam. SetSignal is nil-safe (lazy map init) and
// records the transitions the operator drives per signal.
func TestSetSignalTransitions(t *testing.T) {
	var st OTelConfigStatus // zero value: Signals is nil

	// First write must not panic on the nil map (D-AC2 seam).
	st.SetSignal("traces", SignalStatePending, "")
	if got := st.Signals["traces"].State; got != SignalStatePending {
		t.Fatalf("initial state = %q want pending", got)
	}

	// pending → healthy (export succeeded): detail cleared.
	st.SetSignal("traces", SignalStateHealthy, "")
	if got := st.Signals["traces"]; got.State != SignalStateHealthy || got.Detail != "" {
		t.Fatalf("after success = %+v want healthy/empty", got)
	}

	// healthy → erroring (export failed): detail carries the reason.
	st.SetSignal("traces", SignalStateErroring, "endpoint unreachable")
	if got := st.Signals["traces"]; got.State != SignalStateErroring || got.Detail != "endpoint unreachable" {
		t.Fatalf("after failure = %+v want erroring/endpoint unreachable", got)
	}

	// erroring → disabled (exporter removed): stale detail is replaced, not kept.
	st.SetSignal("traces", SignalStateDisabled, "")
	if got := st.Signals["traces"]; got.State != SignalStateDisabled || got.Detail != "" {
		t.Fatalf("after disable = %+v want disabled/empty", got)
	}

	// Signals are independent keys.
	st.SetSignal("metrics", SignalStateHealthy, "")
	if len(st.Signals) != 2 {
		t.Fatalf("expected 2 signals, got %d", len(st.Signals))
	}
}

// D-AC3: the reconciler must pass a secret-free detail, but guard the contract at
// the type level too — whatever detail is set is exactly what serializes (no
// hidden field can smuggle a token), so the reason string is the only surface a
// reviewer must keep clean.
func TestSignalStatusDetailIsTheOnlyTextField(t *testing.T) {
	st := OTelConfigStatus{}
	st.SetSignal("logs", SignalStateErroring, "auth Secret 'otlp-token' not found")

	body, err := json.Marshal(st.Signals["logs"])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Assert the serialized shape is exactly {state, detail} — no extra field
	// exists through which a token value could ever be smuggled onto status.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for k := range fields {
		if k != "state" && k != "detail" {
			t.Fatalf("unexpected field %q in SignalStatus json: %s", k, body)
		}
	}
	if _, ok := fields["state"]; !ok {
		t.Fatalf("state field missing: %s", body)
	}
}
