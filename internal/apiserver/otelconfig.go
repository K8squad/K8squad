package apiserver

import (
	"context"
	"errors"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// ============================================================================
// Story A / 13.8 (ISI-2917, child of ISI-3586 under confirmed Option A) — the
// apiserver read model that lets the Settings page STOP rendering "no exporters"
// once a cluster-scoped OTelConfig CR exists:
//
//	GET /api/otelconfig → OtelConfigWire (console/lib/otelconfig.ts) | 404 (opt-in default)
//
// ============================================================================
//
// Read-model ONLY — no writes here (the compose/write surface is separate). The
// path is exactly "/api/otelconfig" (no company/team prefix); the BFF
// (console/app/api/otelconfig/route.ts) proxies it verbatim. Client fromWire()
// reconstructs the form from this body without error (A-AC1).
//
// The OTelConfig CRD is CLUSTER-scoped (telemetry routing is a platform concern,
// not a Team concern), so there is no tenancy filter here — only the §13 authz
// choke point the route inherits, exactly like the other read models.
//
// Ratified W-decisions this mapper implements (ISI-3586 Story Writer, adopting
// the ISI-3557 doc under Option A):
//
//	W1 protocol — CRD grpc → wire "grpc"; CRD http/protobuf AND http/json both
//	   canonicalize to the wire's "http" (the wire enum is grpc|http for v1;
//	   http/json is reachable only via a direct CR edit, documented & acceptable).
//	   The client's fromWire() already collapses anything non-grpc to "http".
//	W2 auth   — wire authSecretRef carries the Secret NAME, or "namespace/name"
//	   when the CR sets a namespace. The Secret KEY (default "token") is a
//	   consumer concern and is NOT carried on the wire. This handler reads the CR
//	   ONLY — it never reads Secret contents, so no token value can ever be
//	   emitted (A-AC3).
//	W3 sampling — the CRD's SamplingConfig maps to the wire's scalar ratio:
//	   nil → omitted; always_off → 0; always_on → 1; probabilistic → the ratio.
//
// status.signals (W4 healthy|erroring|pending|disabled) is DELIBERATELY not
// emitted: the OTelConfig CRD carries no status.signals yet — Story D adds it.
// This mapper surfaces it once present; until then the wire status is omitted
// and the Settings page shows the config without a per-signal health chip.

// ErrOTelConfigNotFound is the sentinel the source returns when no OTelConfig CR
// exists. It maps to the 404 the BFF/form treats as the opt-in "nothing
// configured" default (A-AC2) — expected, not an error.
var ErrOTelConfigNotFound = errors.New("no OTelConfig configured")

// OTelConfigSource is the seam the handler reads through: the single current
// cluster-scoped OTelConfig, or ErrOTelConfigNotFound. Nil source ⇒ the route
// answers the documented 501, exactly like the other read models (A-AC5).
type OTelConfigSource interface {
	Get(ctx context.Context) (*ksquadv1.OTelConfig, error)
}

// ClientOTelConfigSource is the production OTelConfigSource over any client.Reader
// (the shared informer cache in the host; a fake client in tests). It applies the
// deterministic multiple-CR pick (A-AC6) so the read is stable regardless of CR
// creation order.
type ClientOTelConfigSource struct {
	reader client.Reader
}

// NewClientOTelConfigSource builds the read model over a client.Reader whose
// scheme has api/v1alpha1 registered (the informer cache in prod; a fake client
// in tests).
func NewClientOTelConfigSource(r client.Reader) *ClientOTelConfigSource {
	return &ClientOTelConfigSource{reader: r}
}

// Get lists the cluster-scoped OTelConfigs and returns the deterministic pick, or
// ErrOTelConfigNotFound when none exist.
func (s *ClientOTelConfigSource) Get(ctx context.Context) (*ksquadv1.OTelConfig, error) {
	var list ksquadv1.OTelConfigList
	if err := s.reader.List(ctx, &list); err != nil {
		return nil, err
	}
	return pickOTelConfig(list.Items)
}

// pickOTelConfig implements A-AC6: with more than one OTelConfig the read is still
// deterministic and NEVER 500 — prefer the CR named "default", else the
// lexically-first by name. Zero CRs ⇒ ErrOTelConfigNotFound (the 404 default).
func pickOTelConfig(items []ksquadv1.OTelConfig) (*ksquadv1.OTelConfig, error) {
	if len(items) == 0 {
		return nil, ErrOTelConfigNotFound
	}
	best := &items[0]
	for i := range items {
		c := &items[i]
		if c.Name == "default" {
			return c, nil
		}
		if c.Name < best.Name {
			best = c
		}
	}
	return best, nil
}

// ── CRD → wire mapping (unit-testable without kube) ─────────────────────────

// signalWire mirrors console/lib/otelconfig.ts SignalWire. sampling is a pointer
// so 0 (always_off) is EMITTED, not dropped by omitempty (omitempty on a pointer
// checks nil, not the zero value).
type signalWire struct {
	Endpoint           string            `json:"endpoint,omitempty"`
	Protocol           string            `json:"protocol,omitempty"`
	AuthSecretRef      string            `json:"authSecretRef,omitempty"`
	ResourceAttributes map[string]string `json:"resourceAttributes,omitempty"`
	Sampling           *float64          `json:"sampling,omitempty"`
}

// otelConfigSpecWire mirrors OtelConfigWire["spec"]. An unconfigured signal is
// omitted entirely (absence == "no exporter for this signal", the opt-in state
// the client's fromWire() reads).
type otelConfigSpecWire struct {
	Traces  *signalWire `json:"traces,omitempty"`
	Metrics *signalWire `json:"metrics,omitempty"`
	Logs    *signalWire `json:"logs,omitempty"`
}

// OtelConfigWire is the client wire shape (console/lib/otelconfig.ts OtelConfigWire).
// spec is a value (always serialized) so fromWire()'s `wire?.spec ?? {}` reads a
// present object even when every signal is unconfigured. status is omitted until
// Story D adds status.signals to the CRD (see file header).
type OtelConfigWire struct {
	APIVersion string             `json:"apiVersion,omitempty"`
	Kind       string             `json:"kind,omitempty"`
	Spec       otelConfigSpecWire `json:"spec"`
}

// crdToWire is the deterministic CRD→wire projection (A-AC4). Pure — no kube, no
// Secret reads — so it is fully table-testable.
func crdToWire(cr *ksquadv1.OTelConfig) OtelConfigWire {
	return OtelConfigWire{
		APIVersion: ksquadv1.GroupVersion.String(),
		Kind:       "OTelConfig",
		Spec: otelConfigSpecWire{
			Traces:  signalToWire(cr.Spec.Traces),
			Metrics: signalToWire(cr.Spec.Metrics),
			Logs:    signalToWire(cr.Spec.Logs),
		},
	}
}

// signalToWire maps one CRD SignalRouting to its wire shape; nil in ⇒ nil out
// (the signal is omitted from spec).
func signalToWire(s *ksquadv1.SignalRouting) *signalWire {
	if s == nil {
		return nil
	}
	w := &signalWire{
		Endpoint:           s.Endpoint,
		Protocol:           wireProtocol(s.Protocol),
		ResourceAttributes: s.ResourceAttributes,
		Sampling:           wireSampling(s.Sampling),
	}
	if s.Auth != nil {
		w.AuthSecretRef = wireAuthRef(s.Auth)
	}
	return w
}

// wireProtocol implements W1: grpc stays grpc; every http/* variant canonicalizes
// to the wire's "http".
func wireProtocol(p ksquadv1.ExportProtocol) string {
	if p == ksquadv1.ExportProtocolGRPC {
		return "grpc"
	}
	return "http"
}

// wireAuthRef implements W2: NAME, or "namespace/name" when the CR sets a
// namespace. The Secret KEY never crosses the wire, and no Secret contents are
// read (A-AC3).
func wireAuthRef(a *ksquadv1.SecretKeyReference) string {
	if a.Namespace != "" {
		return a.Namespace + "/" + a.Name
	}
	return a.Name
}

// wireSampling implements W3: nil ⇒ omit; always_off ⇒ 0; always_on ⇒ 1;
// probabilistic ⇒ the ratio (a probabilistic sampler without a ratio is invalid
// per the CRD's CEL, but we omit rather than emit a misleading value).
func wireSampling(s *ksquadv1.SamplingConfig) *float64 {
	if s == nil {
		return nil
	}
	switch s.Type {
	case ksquadv1.SamplingTypeAlwaysOff:
		return float64Ptr(0)
	case ksquadv1.SamplingTypeAlwaysOn:
		return float64Ptr(1)
	case ksquadv1.SamplingTypeProbabilistic:
		if s.Ratio != nil {
			r := *s.Ratio
			return &r
		}
		return nil
	default:
		return nil
	}
}

func float64Ptr(f float64) *float64 { return &f }

// ── handler ─────────────────────────────────────────────────────────────────

// otelConfig is the handler behind GET /api/otelconfig. A missing CR is the
// opt-in 404 default (A-AC2), not an error; a read failure answers 502 (same
// discipline as the org/onboarding handlers). It reads the CR only and emits the
// CRD→wire projection — no token value can appear in the body (A-AC3).
func (s *Server) otelConfig(source OTelConfigSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cr, err := source.Get(r.Context())
		if errors.Is(err, ErrOTelConfigNotFound) {
			writeJSONError(w, http.StatusNotFound, "no OTelConfig configured")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "otel-config read model unavailable")
			return
		}
		writeJSON(w, http.StatusOK, crdToWire(cr))
	}
}
