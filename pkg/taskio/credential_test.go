package taskio

import (
	"reflect"
	"testing"
)

func fullCredential() RunCredential {
	return RunCredential{
		CoordURL:    "http://coord.ksquad-system.svc:8080",
		Token:       "tok-abc",
		WorkItemID:  "wi-1",
		RunID:       "run-1",
		TraceParent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		TraceState:  "ksquad=1",
	}
}

// EnvKV emits every non-empty field in the fixed order, as NAME=value.
func TestRunCredential_EnvKV_Order(t *testing.T) {
	want := []string{
		"KSQUAD_COORD_URL=http://coord.ksquad-system.svc:8080",
		"KSQUAD_COORD_TOKEN=tok-abc",
		"WORK_ITEM_ID=wi-1",
		"RUN_ID=run-1",
		"TRACEPARENT=00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		"TRACESTATE=ksquad=1",
	}
	if got := fullCredential().EnvKV(); !reflect.DeepEqual(got, want) {
		t.Fatalf("EnvKV()\n got %q\nwant %q", got, want)
	}
}

// The shim path populates only the four task-io vars (trace is emitted out of
// band); EnvKV must then omit the empty trace fields, never emit a bare NAME=.
func TestRunCredential_EnvKV_OmitsEmpty(t *testing.T) {
	c := RunCredential{CoordURL: "u", Token: "t", WorkItemID: "wi", RunID: "r"}
	want := []string{
		"KSQUAD_COORD_URL=u",
		"KSQUAD_COORD_TOKEN=t",
		"WORK_ITEM_ID=wi",
		"RUN_ID=r",
	}
	if got := c.EnvKV(); !reflect.DeepEqual(got, want) {
		t.Fatalf("EnvKV() with empty trace\n got %q\nwant %q", got, want)
	}
	if got := (RunCredential{}).EnvKV(); len(got) != 0 {
		t.Fatalf("empty credential EnvKV() = %q, want none", got)
	}
}

// SecretData keys/values match EnvKV exactly — the whole point of the shared
// struct is that the env carrier (topology 1) and the projected-Secret carrier
// (topology 2, ADR-0007) deliver byte-identical content. If these ever drift the
// two dispatch topologies would hand agents different credentials.
func TestRunCredential_SecretData_MatchesEnvKV(t *testing.T) {
	c := fullCredential()
	secret := c.SecretData()

	for _, kv := range c.EnvKV() {
		// split on the first '=' — values (e.g. tracestate) may contain '='.
		eq := -1
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				eq = i
				break
			}
		}
		if eq < 0 {
			t.Fatalf("EnvKV entry has no '=': %q", kv)
		}
		k, v := kv[:eq], kv[eq+1:]
		got, ok := secret[k]
		if !ok {
			t.Errorf("SecretData missing key %q present in EnvKV", k)
			continue
		}
		if string(got) != v {
			t.Errorf("SecretData[%q]=%q, EnvKV has %q — carriers drifted", k, got, v)
		}
	}
	if len(secret) != len(c.EnvKV()) {
		t.Errorf("SecretData has %d keys, EnvKV has %d entries — key sets differ", len(secret), len(c.EnvKV()))
	}
}

// SecretData omits empty fields and is always non-nil (callers range/assign).
func TestRunCredential_SecretData_OmitsEmpty(t *testing.T) {
	data := (RunCredential{Token: "t"}).SecretData()
	if data == nil {
		t.Fatal("SecretData() = nil, want non-nil empty-safe map")
	}
	if len(data) != 1 || string(data[EnvCoordToken]) != "t" {
		t.Fatalf("SecretData() = %v, want only %s=t", data, EnvCoordToken)
	}
}
