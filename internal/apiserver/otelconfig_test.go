package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// float64 helper local to tests to keep the table literals compact.
func f64(v float64) *float64 { return &v }

// ── A-AC4: table-driven CRD→wire mapping ─────────────────────────────────────
// Covers http/protobuf, http/json, grpc, all three sampling types, and an
// auth-with-namespace case, plus the W1/W2/W3 default resolutions.
func TestCRDToWire(t *testing.T) {
	cases := []struct {
		name string
		spec ksquadv1.OTelConfigSpec
		want otelConfigSpecWire
	}{
		{
			name: "http/protobuf canonicalizes to wire http (W1)",
			spec: ksquadv1.OTelConfigSpec{
				Traces: &ksquadv1.SignalRouting{
					Endpoint: "https://otlp.example/v1/traces",
					Protocol: ksquadv1.ExportProtocolHTTPProtobuf,
				},
			},
			want: otelConfigSpecWire{
				Traces: &signalWire{Endpoint: "https://otlp.example/v1/traces", Protocol: "http"},
			},
		},
		{
			name: "http/json also canonicalizes to wire http (W1, direct-edit path)",
			spec: ksquadv1.OTelConfigSpec{
				Metrics: &ksquadv1.SignalRouting{
					Endpoint: "https://otlp.example/v1/metrics",
					Protocol: ksquadv1.ExportProtocolHTTPJSON,
				},
			},
			want: otelConfigSpecWire{
				Metrics: &signalWire{Endpoint: "https://otlp.example/v1/metrics", Protocol: "http"},
			},
		},
		{
			name: "grpc stays grpc (W1)",
			spec: ksquadv1.OTelConfigSpec{
				Logs: &ksquadv1.SignalRouting{
					Endpoint: "otlp.example:4317",
					Protocol: ksquadv1.ExportProtocolGRPC,
				},
			},
			want: otelConfigSpecWire{
				Logs: &signalWire{Endpoint: "otlp.example:4317", Protocol: "grpc"},
			},
		},
		{
			name: "sampling always_off → 0 (W3)",
			spec: ksquadv1.OTelConfigSpec{
				Traces: &ksquadv1.SignalRouting{
					Endpoint: "otlp:4317", Protocol: ksquadv1.ExportProtocolGRPC,
					Sampling: &ksquadv1.SamplingConfig{Type: ksquadv1.SamplingTypeAlwaysOff},
				},
			},
			want: otelConfigSpecWire{
				Traces: &signalWire{Endpoint: "otlp:4317", Protocol: "grpc", Sampling: f64(0)},
			},
		},
		{
			name: "sampling always_on → 1 (W3)",
			spec: ksquadv1.OTelConfigSpec{
				Traces: &ksquadv1.SignalRouting{
					Endpoint: "otlp:4317", Protocol: ksquadv1.ExportProtocolGRPC,
					Sampling: &ksquadv1.SamplingConfig{Type: ksquadv1.SamplingTypeAlwaysOn},
				},
			},
			want: otelConfigSpecWire{
				Traces: &signalWire{Endpoint: "otlp:4317", Protocol: "grpc", Sampling: f64(1)},
			},
		},
		{
			name: "sampling probabilistic → ratio (W3)",
			spec: ksquadv1.OTelConfigSpec{
				Traces: &ksquadv1.SignalRouting{
					Endpoint: "otlp:4317", Protocol: ksquadv1.ExportProtocolGRPC,
					Sampling: &ksquadv1.SamplingConfig{Type: ksquadv1.SamplingTypeProbabilistic, Ratio: f64(0.25)},
				},
			},
			want: otelConfigSpecWire{
				Traces: &signalWire{Endpoint: "otlp:4317", Protocol: "grpc", Sampling: f64(0.25)},
			},
		},
		{
			name: "auth with namespace → ns/name (W2); key never crosses the wire",
			spec: ksquadv1.OTelConfigSpec{
				Traces: &ksquadv1.SignalRouting{
					Endpoint: "https://otlp.example/v1/traces",
					Protocol: ksquadv1.ExportProtocolHTTPProtobuf,
					Auth:     &ksquadv1.SecretKeyReference{Name: "otlp-token", Key: "token", Namespace: "obs"},
				},
			},
			want: otelConfigSpecWire{
				Traces: &signalWire{
					Endpoint:      "https://otlp.example/v1/traces",
					Protocol:      "http",
					AuthSecretRef: "obs/otlp-token",
				},
			},
		},
		{
			name: "auth without namespace → bare name (W2)",
			spec: ksquadv1.OTelConfigSpec{
				Traces: &ksquadv1.SignalRouting{
					Endpoint: "https://otlp.example/v1/traces",
					Protocol: ksquadv1.ExportProtocolHTTPProtobuf,
					Auth:     &ksquadv1.SecretKeyReference{Name: "otlp-token", Key: "token"},
				},
			},
			want: otelConfigSpecWire{
				Traces: &signalWire{
					Endpoint:      "https://otlp.example/v1/traces",
					Protocol:      "http",
					AuthSecretRef: "otlp-token",
				},
			},
		},
		{
			name: "resourceAttributes carried through; nil sampling omitted",
			spec: ksquadv1.OTelConfigSpec{
				Traces: &ksquadv1.SignalRouting{
					Endpoint: "otlp:4317", Protocol: ksquadv1.ExportProtocolGRPC,
					ResourceAttributes: map[string]string{"deployment.environment": "prod"},
				},
			},
			want: otelConfigSpecWire{
				Traces: &signalWire{
					Endpoint: "otlp:4317", Protocol: "grpc",
					ResourceAttributes: map[string]string{"deployment.environment": "prod"},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := crdToWire(&ksquadv1.OTelConfig{Spec: tc.spec})
			if got.APIVersion != "ksquad.io/v1alpha1" || got.Kind != "OTelConfig" {
				t.Fatalf("apiVersion/kind: got %q/%q", got.APIVersion, got.Kind)
			}
			assertSignal(t, "traces", got.Spec.Traces, tc.want.Traces)
			assertSignal(t, "metrics", got.Spec.Metrics, tc.want.Metrics)
			assertSignal(t, "logs", got.Spec.Logs, tc.want.Logs)
		})
	}
}

func assertSignal(t *testing.T, key string, got, want *signalWire) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("%s: presence mismatch got=%v want=%v", key, got, want)
	}
	if got == nil {
		return
	}
	if got.Endpoint != want.Endpoint || got.Protocol != want.Protocol || got.AuthSecretRef != want.AuthSecretRef {
		t.Fatalf("%s: got %+v want %+v", key, got, want)
	}
	if (got.Sampling == nil) != (want.Sampling == nil) {
		t.Fatalf("%s: sampling presence got=%v want=%v", key, got.Sampling, want.Sampling)
	}
	if got.Sampling != nil && *got.Sampling != *want.Sampling {
		t.Fatalf("%s: sampling got %v want %v", key, *got.Sampling, *want.Sampling)
	}
	if len(got.ResourceAttributes) != len(want.ResourceAttributes) {
		t.Fatalf("%s: resourceAttributes got %v want %v", key, got.ResourceAttributes, want.ResourceAttributes)
	}
	for k, v := range want.ResourceAttributes {
		if got.ResourceAttributes[k] != v {
			t.Fatalf("%s: resourceAttributes[%s] got %q want %q", key, k, got.ResourceAttributes[k], v)
		}
	}
}

// A-AC3: the marshaled body carries only the reference name — never a token value
// and never the Secret key. (The handler reads the CR only; this pins the wire shape.)
func TestCRDToWireNeverEmitsTokenOrKey(t *testing.T) {
	wire := crdToWire(&ksquadv1.OTelConfig{Spec: ksquadv1.OTelConfigSpec{
		Traces: &ksquadv1.SignalRouting{
			Endpoint: "https://otlp.example/v1/traces", Protocol: ksquadv1.ExportProtocolHTTPProtobuf,
			Auth: &ksquadv1.SecretKeyReference{Name: "otlp-token", Key: "super-secret-key-name", Namespace: "obs"},
		},
	}})
	body, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if s := string(body); strings.Contains(s, "super-secret-key-name") {
		t.Fatalf("body leaked the Secret key: %s", s)
	}
}

// D-AC1/D-AC4: the mapper surfaces status.signals verbatim so the Console
// "Export state" card can render per-signal health; absent status ⇒ omitted.
func TestCRDToWireStatusSignals(t *testing.T) {
	t.Run("no signals reported → status omitted", func(t *testing.T) {
		wire := crdToWire(&ksquadv1.OTelConfig{})
		if wire.Status != nil {
			t.Fatalf("status should be nil until the operator reports a signal, got %+v", wire.Status)
		}
		body, _ := json.Marshal(wire)
		if strings.Contains(string(body), "\"status\"") {
			t.Fatalf("empty status must not serialize: %s", body)
		}
	})

	t.Run("reported signals surface state+detail", func(t *testing.T) {
		cr := &ksquadv1.OTelConfig{}
		cr.Status.SetSignal("traces", ksquadv1.SignalStateHealthy, "")
		cr.Status.SetSignal("metrics", ksquadv1.SignalStateErroring, "endpoint unreachable")
		cr.Status.SetSignal("logs", ksquadv1.SignalStateDisabled, "")

		wire := crdToWire(cr)
		if wire.Status == nil || len(wire.Status.Signals) != 3 {
			t.Fatalf("expected 3 signals on wire, got %+v", wire.Status)
		}
		if got := wire.Status.Signals["traces"].State; got != "healthy" {
			t.Fatalf("traces state = %q want healthy", got)
		}
		if got := wire.Status.Signals["metrics"]; got.State != "erroring" || got.Detail != "endpoint unreachable" {
			t.Fatalf("metrics = %+v want erroring/endpoint unreachable", got)
		}
		if got := wire.Status.Signals["logs"].State; got != "disabled" {
			t.Fatalf("logs state = %q want disabled", got)
		}
	})
}

// A-AC6: deterministic multiple-CR pick — "default" wins; else lexically-first; never errors.
func TestPickOTelConfig(t *testing.T) {
	mk := func(name string) ksquadv1.OTelConfig {
		return ksquadv1.OTelConfig{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}
	t.Run("empty → not found", func(t *testing.T) {
		if _, err := pickOTelConfig(nil); err != ErrOTelConfigNotFound {
			t.Fatalf("got %v want ErrOTelConfigNotFound", err)
		}
	})
	t.Run("default wins regardless of order", func(t *testing.T) {
		got, err := pickOTelConfig([]ksquadv1.OTelConfig{mk("aaa"), mk("default"), mk("zzz")})
		if err != nil || got.Name != "default" {
			t.Fatalf("got %v err %v", got.Name, err)
		}
	})
	t.Run("no default → lexically-first", func(t *testing.T) {
		got, err := pickOTelConfig([]ksquadv1.OTelConfig{mk("zebra"), mk("apple"), mk("mango")})
		if err != nil || got.Name != "apple" {
			t.Fatalf("got %v err %v", got.Name, err)
		}
	})
}

// ── handler wiring ───────────────────────────────────────────────────────────

func testOtelConfigServer(t *testing.T, source OTelConfigSource) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice"},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		OTelConfig:    source,
	})
	return srv.Handler()
}

// staticSource is a trivial OTelConfigSource over a fixed CR (or none).
type staticSource struct {
	cr *ksquadv1.OTelConfig
}

func (s staticSource) Get(context.Context) (*ksquadv1.OTelConfig, error) {
	if s.cr == nil {
		return nil, ErrOTelConfigNotFound
	}
	return s.cr, nil
}

// A-AC1: 200 with an OtelConfigWire body a client fromWire() can reconstruct.
func TestOtelConfigHandlerOK(t *testing.T) {
	cr := &ksquadv1.OTelConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: ksquadv1.OTelConfigSpec{
			Traces: &ksquadv1.SignalRouting{
				Endpoint: "https://otlp.example/v1/traces", Protocol: ksquadv1.ExportProtocolHTTPProtobuf,
				Auth: &ksquadv1.SecretKeyReference{Name: "otlp-token", Key: "token"},
			},
		},
	}
	h := testOtelConfigServer(t, staticSource{cr: cr})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/otelconfig", nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var wire OtelConfigWire
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if wire.Spec.Traces == nil || wire.Spec.Traces.Protocol != "http" || wire.Spec.Traces.AuthSecretRef != "otlp-token" {
		t.Fatalf("body: %+v", wire.Spec.Traces)
	}
}

// A-AC2: no CR ⇒ 404 (the opt-in "nothing configured" default, not an error).
func TestOtelConfigHandler404(t *testing.T) {
	h := testOtelConfigServer(t, staticSource{cr: nil})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/otelconfig", nil), devToken))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no CR: got %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// A-AC5: nil source ⇒ documented 501.
func TestOtelConfigNilSource501(t *testing.T) {
	h := testOtelConfigServer(t, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/otelconfig", nil), devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil source: got %d, want 501 (body %s)", rec.Code, rec.Body.String())
	}
}

// Unauthenticated ⇒ 401 at the §13 choke point.
func TestOtelConfigUnauthenticated(t *testing.T) {
	h := testOtelConfigServer(t, staticSource{cr: &ksquadv1.OTelConfig{}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/otelconfig", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: got %d, want 401", rec.Code)
	}
}

// A-AC6 end-to-end over a fake client: >1 CR is deterministic and never 500.
func TestClientOTelConfigSourcePicksDefault(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(
		&ksquadv1.OTelConfig{ObjectMeta: metav1.ObjectMeta{Name: "zzz"},
			Spec: ksquadv1.OTelConfigSpec{Logs: &ksquadv1.SignalRouting{Endpoint: "a:4317", Protocol: ksquadv1.ExportProtocolGRPC}}},
		&ksquadv1.OTelConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec: ksquadv1.OTelConfigSpec{Logs: &ksquadv1.SignalRouting{Endpoint: "b:4317", Protocol: ksquadv1.ExportProtocolGRPC}}},
	).Build()
	got, err := NewClientOTelConfigSource(c).Get(context.Background())
	if err != nil || got.Name != "default" {
		t.Fatalf("got %v err %v", got, err)
	}
}
