package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// ============================================================================
// Write half (ISI-3954) — wireToCRD + PUT/POST handler
// ============================================================================

const otelAdminToken = "otel-admin-token"

// ── wireToCRD: the inverse mapping (W1/W2/W3), table-driven ──────────────────
func TestWireToCRD(t *testing.T) {
	cases := []struct {
		name string
		spec otelConfigSpecWire
		want ksquadv1.OTelConfigSpec
	}{
		{
			name: "wire http → canonical http/protobuf (W1)",
			spec: otelConfigSpecWire{Traces: &signalWire{Endpoint: "https://otlp.example/v1/traces", Protocol: "http"}},
			want: ksquadv1.OTelConfigSpec{Traces: &ksquadv1.SignalRouting{
				Endpoint: "https://otlp.example/v1/traces", Protocol: ksquadv1.ExportProtocolHTTPProtobuf,
			}},
		},
		{
			name: "wire grpc stays grpc (W1)",
			spec: otelConfigSpecWire{Logs: &signalWire{Endpoint: "otlp:4317", Protocol: "grpc"}},
			want: ksquadv1.OTelConfigSpec{Logs: &ksquadv1.SignalRouting{
				Endpoint: "otlp:4317", Protocol: ksquadv1.ExportProtocolGRPC,
			}},
		},
		{
			name: "absent protocol defaults to http/protobuf (W1)",
			spec: otelConfigSpecWire{Metrics: &signalWire{Endpoint: "https://otlp.example/v1/metrics"}},
			want: ksquadv1.OTelConfigSpec{Metrics: &ksquadv1.SignalRouting{
				Endpoint: "https://otlp.example/v1/metrics", Protocol: ksquadv1.ExportProtocolHTTPProtobuf,
			}},
		},
		{
			name: "auth ns/name → namespace+name, key re-materialized to token (W2)",
			spec: otelConfigSpecWire{Traces: &signalWire{
				Endpoint: "otlp:4317", Protocol: "grpc", AuthSecretRef: "obs/otlp-token",
			}},
			want: ksquadv1.OTelConfigSpec{Traces: &ksquadv1.SignalRouting{
				Endpoint: "otlp:4317", Protocol: ksquadv1.ExportProtocolGRPC,
				Auth: &ksquadv1.SecretKeyReference{Name: "otlp-token", Namespace: "obs", Key: "token"},
			}},
		},
		{
			name: "auth bare name → name only, key token (W2)",
			spec: otelConfigSpecWire{Traces: &signalWire{
				Endpoint: "otlp:4317", Protocol: "grpc", AuthSecretRef: "otlp-token",
			}},
			want: ksquadv1.OTelConfigSpec{Traces: &ksquadv1.SignalRouting{
				Endpoint: "otlp:4317", Protocol: ksquadv1.ExportProtocolGRPC,
				Auth: &ksquadv1.SecretKeyReference{Name: "otlp-token", Key: "token"},
			}},
		},
		{
			name: "sampling 0 → always_off (W3)",
			spec: otelConfigSpecWire{Traces: &signalWire{Endpoint: "otlp:4317", Protocol: "grpc", Sampling: f64(0)}},
			want: ksquadv1.OTelConfigSpec{Traces: &ksquadv1.SignalRouting{
				Endpoint: "otlp:4317", Protocol: ksquadv1.ExportProtocolGRPC,
				Sampling: &ksquadv1.SamplingConfig{Type: ksquadv1.SamplingTypeAlwaysOff},
			}},
		},
		{
			name: "sampling 1 → always_on (W3)",
			spec: otelConfigSpecWire{Traces: &signalWire{Endpoint: "otlp:4317", Protocol: "grpc", Sampling: f64(1)}},
			want: ksquadv1.OTelConfigSpec{Traces: &ksquadv1.SignalRouting{
				Endpoint: "otlp:4317", Protocol: ksquadv1.ExportProtocolGRPC,
				Sampling: &ksquadv1.SamplingConfig{Type: ksquadv1.SamplingTypeAlwaysOn},
			}},
		},
		{
			name: "sampling ratio → probabilistic (W3)",
			spec: otelConfigSpecWire{Traces: &signalWire{Endpoint: "otlp:4317", Protocol: "grpc", Sampling: f64(0.25)}},
			want: ksquadv1.OTelConfigSpec{Traces: &ksquadv1.SignalRouting{
				Endpoint: "otlp:4317", Protocol: ksquadv1.ExportProtocolGRPC,
				Sampling: &ksquadv1.SamplingConfig{Type: ksquadv1.SamplingTypeProbabilistic, Ratio: f64(0.25)},
			}},
		},
		{
			name: "resourceAttributes carried; nil sampling omitted",
			spec: otelConfigSpecWire{Traces: &signalWire{
				Endpoint: "otlp:4317", Protocol: "grpc",
				ResourceAttributes: map[string]string{"deployment.environment": "prod"},
			}},
			want: ksquadv1.OTelConfigSpec{Traces: &ksquadv1.SignalRouting{
				Endpoint: "otlp:4317", Protocol: ksquadv1.ExportProtocolGRPC,
				ResourceAttributes: map[string]string{"deployment.environment": "prod"},
			}},
		},
		{
			name: "signal with empty endpoint → omitted (per-signal nil)",
			spec: otelConfigSpecWire{Traces: &signalWire{Protocol: "grpc"}},
			want: ksquadv1.OTelConfigSpec{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wireToCRD(OtelConfigWire{Spec: tc.spec})
			if got.Name != otelConfigCanonicalName {
				t.Fatalf("name: got %q want %q", got.Name, otelConfigCanonicalName)
			}
			if got.Namespace != "" {
				t.Fatalf("cluster-scoped CR must have no namespace, got %q", got.Namespace)
			}
			assertRouting(t, "traces", got.Spec.Traces, tc.want.Traces)
			assertRouting(t, "metrics", got.Spec.Metrics, tc.want.Metrics)
			assertRouting(t, "logs", got.Spec.Logs, tc.want.Logs)
		})
	}
}

func assertRouting(t *testing.T, key string, got, want *ksquadv1.SignalRouting) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("%s: presence mismatch got=%v want=%v", key, got, want)
	}
	if got == nil {
		return
	}
	if got.Endpoint != want.Endpoint || got.Protocol != want.Protocol {
		t.Fatalf("%s: endpoint/protocol got %q/%q want %q/%q", key, got.Endpoint, got.Protocol, want.Endpoint, want.Protocol)
	}
	if (got.Auth == nil) != (want.Auth == nil) {
		t.Fatalf("%s: auth presence got=%v want=%v", key, got.Auth, want.Auth)
	}
	if got.Auth != nil && (got.Auth.Name != want.Auth.Name || got.Auth.Namespace != want.Auth.Namespace || got.Auth.Key != want.Auth.Key) {
		t.Fatalf("%s: auth got %+v want %+v", key, got.Auth, want.Auth)
	}
	if (got.Sampling == nil) != (want.Sampling == nil) {
		t.Fatalf("%s: sampling presence got=%v want=%v", key, got.Sampling, want.Sampling)
	}
	if got.Sampling != nil {
		if got.Sampling.Type != want.Sampling.Type {
			t.Fatalf("%s: sampling type got %q want %q", key, got.Sampling.Type, want.Sampling.Type)
		}
		if (got.Sampling.Ratio == nil) != (want.Sampling.Ratio == nil) {
			t.Fatalf("%s: sampling ratio presence got=%v want=%v", key, got.Sampling.Ratio, want.Sampling.Ratio)
		}
		if got.Sampling.Ratio != nil && *got.Sampling.Ratio != *want.Sampling.Ratio {
			t.Fatalf("%s: sampling ratio got %v want %v", key, *got.Sampling.Ratio, *want.Sampling.Ratio)
		}
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

// crdToWire ∘ wireToCRD is stable on the wire's own vocabulary (grpc/http, the
// scalar sampling ratio, ns/name auth): a save-then-read round-trips unchanged.
func TestWireToCRDRoundTripsThroughCRDToWire(t *testing.T) {
	in := OtelConfigWire{Spec: otelConfigSpecWire{
		Traces: &signalWire{
			Endpoint: "https://otlp.example/v1/traces", Protocol: "http",
			AuthSecretRef: "obs/otlp-token", Sampling: f64(0.5),
			ResourceAttributes: map[string]string{"service.namespace": "ksquad"},
		},
		Logs: &signalWire{Endpoint: "otlp:4317", Protocol: "grpc"},
	}}
	out := crdToWire(wireToCRD(in))
	assertSignal(t, "traces", out.Spec.Traces, in.Spec.Traces)
	assertSignal(t, "logs", out.Spec.Logs, in.Spec.Logs)
	assertSignal(t, "metrics", out.Spec.Metrics, nil)
}

// ── handler wiring ───────────────────────────────────────────────────────────

func testOtelConfigWriteServer(t *testing.T, writer *OTelConfigWriteService) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		otelAdminToken: {Principal: "user:admin", IsAdmin: true},
		devToken:       {Principal: "user:alice"}, // authenticated non-admin
	}}
	srv := NewServer(Options{
		Authenticator:    NewCookieAuthenticator(resolver),
		Discussion:       discussion.NewHandler(nil),
		OTelConfigWriter: writer,
	})
	return srv.Handler()
}

func putOtel(token, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPut, "/api/otelconfig", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		withSession(req, token)
	}
	return req
}

const otelValidBody = `{"apiVersion":"ksquad.io/v1alpha1","kind":"OTelConfig","spec":{"traces":{"endpoint":"https://otlp.example/v1/traces","protocol":"http","authSecretRef":"obs/otlp-token","sampling":0.5}}}`

// AC: an admin PUT creates the canonical "default" CR (201, revision 1) and the
// stored CR carries the mapped spec.
func TestOtelConfigWriteCreate(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).Build()
	h := testOtelConfigWriteServer(t, NewOTelConfigWriteService(c, nil))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, putOtel(otelAdminToken, otelValidBody))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var res otelConfigWriteResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Operation != "created" || res.Revision != 1 || res.Name != "default" {
		t.Fatalf("result = %+v", res)
	}

	var stored ksquadv1.OTelConfig
	if err := c.Get(context.Background(), client.ObjectKey{Name: "default"}, &stored); err != nil {
		t.Fatalf("stored CR not found: %v", err)
	}
	tr := stored.Spec.Traces
	if tr == nil || tr.Protocol != ksquadv1.ExportProtocolHTTPProtobuf || tr.Endpoint != "https://otlp.example/v1/traces" {
		t.Fatalf("stored traces = %+v", tr)
	}
	if tr.Auth == nil || tr.Auth.Name != "otlp-token" || tr.Auth.Namespace != "obs" || tr.Auth.Key != "token" {
		t.Fatalf("stored auth = %+v", tr.Auth)
	}
	if tr.Sampling == nil || tr.Sampling.Type != ksquadv1.SamplingTypeProbabilistic || tr.Sampling.Ratio == nil || *tr.Sampling.Ratio != 0.5 {
		t.Fatalf("stored sampling = %+v", tr.Sampling)
	}
	if got := stored.Annotations[RevisionAnnotation]; got != "1" {
		t.Fatalf("revision annotation = %q want 1", got)
	}
}

// AC: a second admin PUT edits in place as a NEW revision (200, revision 2).
func TestOtelConfigWriteUpsertRevision(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).Build()
	h := testOtelConfigWriteServer(t, NewOTelConfigWriteService(c, nil))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, putOtel(otelAdminToken, otelValidBody))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first PUT: got %d want 201 (body %s)", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, putOtel(otelAdminToken, `{"spec":{"logs":{"endpoint":"otlp:4317","protocol":"grpc"}}}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("second PUT: got %d want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var res otelConfigWriteResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Operation != "updated" || res.Revision != 2 {
		t.Fatalf("result = %+v want updated/2", res)
	}
	var stored ksquadv1.OTelConfig
	if err := c.Get(context.Background(), client.ObjectKey{Name: "default"}, &stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Spec.Traces != nil || stored.Spec.Logs == nil {
		t.Fatalf("edit did not replace spec: %+v", stored.Spec)
	}
	if got := stored.Annotations[RevisionAnnotation]; got != "2" {
		t.Fatalf("revision annotation = %q want 2", got)
	}
}

// AC: an authenticated NON-admin is 403 (OTelConfig is cluster-scoped/admin-tier).
func TestOtelConfigWriteNonAdminForbidden(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).Build()
	h := testOtelConfigWriteServer(t, NewOTelConfigWriteService(c, nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, putOtel(devToken, otelValidBody))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin: got %d want 403 (body %s)", rec.Code, rec.Body.String())
	}
	var stored ksquadv1.OTelConfig
	if err := c.Get(context.Background(), client.ObjectKey{Name: "default"}, &stored); !apierrors.IsNotFound(err) {
		t.Fatalf("a forbidden write must not touch the cluster; get err = %v", err)
	}
}

// AC: unauthenticated ⇒ 401 at the §13 choke point.
func TestOtelConfigWriteUnauthenticated(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).Build()
	h := testOtelConfigWriteServer(t, NewOTelConfigWriteService(c, nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, putOtel("", otelValidBody))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: got %d want 401", rec.Code)
	}
}

// AC: nil writer ⇒ documented 501 (cluster-less dev run), exactly like the read half.
func TestOtelConfigWriteNilSeam501(t *testing.T) {
	h := testOtelConfigWriteServer(t, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, putOtel(otelAdminToken, otelValidBody))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil writer: got %d want 501 (body %s)", rec.Code, rec.Body.String())
	}
}

// AC: a cross-origin browser PUT is rejected by the shared same-origin guard
// BEFORE the handler (even with an admin cookie).
func TestOtelConfigWriteCrossOriginRejected(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).Build()
	h := testOtelConfigWriteServer(t, NewOTelConfigWriteService(c, nil))
	req := putOtel(otelAdminToken, otelValidBody)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin: got %d want 403 (body %s)", rec.Code, rec.Body.String())
	}
}

// ── error mapping: webhook/CEL 4xx surfaced verbatim ─────────────────────────

// stubApplier is a CRDApplier returning canned errors on Get/Create/Update — used
// to exercise the error-mapping tail (the fake client never runs the webhook/CEL).
type stubApplier struct {
	getErr    error
	createErr error
	updateErr error
}

func (s stubApplier) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return s.getErr
}
func (s stubApplier) Create(context.Context, client.Object, ...client.CreateOption) error {
	return s.createErr
}
func (s stubApplier) Update(context.Context, client.Object, ...client.UpdateOption) error {
	return s.updateErr
}
func (stubApplier) List(context.Context, client.ObjectList, ...client.ListOption) error { return nil }

func TestOtelConfigWriteErrorMapping(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "ksquad.io", Resource: "otelconfigs"}, "default")
	gk := schema.GroupKind{Group: "ksquad.io", Kind: "OTelConfig"}

	cases := []struct {
		name       string
		applier    stubApplier
		wantStatus int
		wantSubstr string // must appear in the verbatim error body
	}{
		{
			name: "CEL/structural Invalid → 422 verbatim",
			applier: stubApplier{getErr: notFound, createErr: apierrors.NewInvalid(gk, "default",
				field.ErrorList{field.Required(field.NewPath("spec"), "at least one signal (traces, metrics, logs) must be configured")})},
			wantStatus: http.StatusUnprocessableEntity,
			wantSubstr: "at least one signal",
		},
		{
			name: "admission-webhook Forbidden → 403 verbatim",
			applier: stubApplier{getErr: notFound, createErr: apierrors.NewForbidden(
				schema.GroupResource{Group: "ksquad.io", Resource: "otelconfigs"}, "default",
				fmt.Errorf("admission webhook denied the request: sampling is only valid on traces"))},
			wantStatus: http.StatusForbidden,
			wantSubstr: "sampling is only valid on traces",
		},
		{
			name:       "conflict → 409 reload-and-retry",
			applier:    stubApplier{getErr: nil, updateErr: apierrors.NewConflict(schema.GroupResource{Group: "ksquad.io", Resource: "otelconfigs"}, "default", fmt.Errorf("resourceVersion mismatch"))},
			wantStatus: http.StatusConflict,
			wantSubstr: "reload and retry",
		},
		{
			name:       "infrastructure error → 502",
			applier:    stubApplier{getErr: notFound, createErr: fmt.Errorf("dial tcp: connection refused")},
			wantStatus: http.StatusBadGateway,
			wantSubstr: "apply failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := testOtelConfigWriteServer(t, NewOTelConfigWriteService(tc.applier, nil))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, putOtel(otelAdminToken, otelValidBody))
			if rec.Code != tc.wantStatus {
				t.Fatalf("got %d want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantSubstr) {
				t.Fatalf("body %q missing %q", rec.Body.String(), tc.wantSubstr)
			}
		})
	}
}

// A best-effort provenance sink records one row per successful apply.
func TestOtelConfigWriteRecordsProvenance(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).Build()
	var rows []map[string]any
	audit := func(_ context.Context, eventType, principal string, payload map[string]any) {
		payload["_event"] = eventType
		payload["_principal"] = principal
		rows = append(rows, payload)
	}
	h := testOtelConfigWriteServer(t, NewOTelConfigWriteService(c, audit))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, putOtel(otelAdminToken, otelValidBody))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d want 201", rec.Code)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 provenance row, got %d", len(rows))
	}
	r := rows[0]
	if r["_event"] != "crd_applied" || r["_principal"] != "user:admin" || r["kind"] != "OTelConfig" || r["operation"] != "created" {
		t.Fatalf("provenance row = %+v", r)
	}
}
