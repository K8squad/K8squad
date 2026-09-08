package apiserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
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
// status.signals (W4 healthy|erroring|pending|disabled) is surfaced here (Story
// D / ISI-3621): the CRD now carries OTelConfigStatus.Signals, populated by the
// export reconciler. This mapper projects it verbatim onto the wire status when
// present; when the operator has reported nothing yet the wire status is omitted
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

// signalStatusWire mirrors console/lib/otelconfig.ts status.signals[key]
// ({state, detail}). detail carries a human-readable, secret-free reason only —
// the source CRD status is populated by the export reconciler under D-AC3, so no
// token value can reach here (this mapper reads the CR, never a Secret).
type signalStatusWire struct {
	State  string `json:"state,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// otelConfigStatusWire mirrors OtelConfigWire["status"]. Keyed by signal name
// ("traces"/"metrics"/"logs"), omitted entirely when the operator has not yet
// reported any signal — the client's `wire.status?.signals?.[key]` reads absence
// as "no health yet" and the Export state card renders nothing for that signal.
type otelConfigStatusWire struct {
	Signals map[string]signalStatusWire `json:"signals,omitempty"`
}

// OtelConfigWire is the client wire shape (console/lib/otelconfig.ts OtelConfigWire).
// spec is a value (always serialized) so fromWire()'s `wire?.spec ?? {}` reads a
// present object even when every signal is unconfigured. status is a pointer,
// emitted only once the operator has reported per-signal health (Story D).
type OtelConfigWire struct {
	APIVersion string                `json:"apiVersion,omitempty"`
	Kind       string                `json:"kind,omitempty"`
	Spec       otelConfigSpecWire    `json:"spec"`
	Status     *otelConfigStatusWire `json:"status,omitempty"`
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
		Status: statusToWire(cr.Status.Signals),
	}
}

// statusToWire maps the CRD's per-signal export health onto the wire status
// (Story D). Empty/nil in ⇒ nil out, so status is omitted until the operator has
// reported at least one signal.
func statusToWire(signals map[string]ksquadv1.SignalStatus) *otelConfigStatusWire {
	if len(signals) == 0 {
		return nil
	}
	out := make(map[string]signalStatusWire, len(signals))
	for key, s := range signals {
		out[key] = signalStatusWire{State: string(s.State), Detail: s.Detail}
	}
	return &otelConfigStatusWire{Signals: out}
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

// ============================================================================
// Write model (ISI-3954, ISI-3949 gap G5) — PUT/POST /api/otelconfig
// ============================================================================
//
// The write sibling of the read model above. The Settings page's "Apply OTLP
// configuration" (OtlpConfigScreen.tsx save() → PUT /api/otelconfig via the BFF)
// returned 405 until this landed because the route registered GET only. This
// wires the write half behind the SAME §13 authz choke point + same-origin CSRF
// guard + bounded body as the compose surface (server.go).
//
// OTelConfig is CLUSTER-scoped (telemetry routing is a platform concern, not a
// Team concern — otelconfig_types.go), so this surface is ADMIN-tier, not the
// per-Project write membership the compose surface enforces. There is no tenancy
// namespace: every write is a NAME-ONLY upsert of the single canonical CR named
// "default" — the same name the read model's deterministic pick prefers
// (pickOTelConfig), so a write and the next read agree on the same object.
//
// It reuses the compose write disciplines verbatim: revision-on-edit (the
// RevisionAnnotation, so an edit is a new revision — never an in-place mutation
// of a snapshot a running export reconciler read), webhook/CEL 4xx surfaced
// VERBATIM (the OTelConfig validating webhook + CRD CEL are the source of truth
// for "at least one signal", endpoint shape, and traces-only sampling — this
// handler never re-implements them), a durable provenance row per apply, and the
// nil-seam documented 501.

// otelConfigCanonicalName is the single cluster-scoped CR every write targets. It
// matches the read model's preferred pick (pickOTelConfig), so the write surface
// and the read surface always converge on the same object regardless of any
// hand-created extras.
const otelConfigCanonicalName = "default"

// defaultOTelAuthSecretKey is the Secret key wireToCRD stamps when the wire
// authSecretRef names only a Secret (W2: the KEY is a consumer concern and never
// crosses the wire, so a write must re-materialize the CRD-required key). It
// matches the read model's documented default and the CRD examples.
const defaultOTelAuthSecretKey = "token" // #nosec G101 -- a Secret KEY name (metadata), not a credential value.

// otelConfigMaxBodyBytes bounds the write body (defense in depth, same discipline
// as the compose/credential write routes). An OTelConfig is three small signal
// blocks; 32 KiB is ample headroom for the 64-property resourceAttributes ceiling
// while staying well bounded.
const otelConfigMaxBodyBytes = 32 << 10

// OTelConfigWriteService is the ISI-3954 write model. It upserts the cluster-scoped
// OTelConfig CR named "default" through a direct controller-runtime client,
// recording a provenance row per apply. A nil service ⇒ the route keeps the
// documented 501 (a cluster-less dev run without a writer client), exactly like
// the read model and the compose surface.
type OTelConfigWriteService struct {
	applier CRDApplier
	audit   ComposeProvenance
}

// NewOTelConfigWriteService builds the write model. applier MUST have api/v1alpha1
// registered on its scheme (the SAME direct write client the compose surface uses
// — one write path into the cluster, never two). audit is the §6.5 provenance sink
// (nil ⇒ provenance is logged only), the SAME coord.audit_log writer compose uses.
func NewOTelConfigWriteService(applier CRDApplier, audit ComposeProvenance) *OTelConfigWriteService {
	return &OTelConfigWriteService{applier: applier, audit: audit}
}

// ── wire → CRD mapping (the inverse of crdToWire; unit-testable without kube) ──

// wireToCRD is the deterministic wire→CRD projection: the inverse of crdToWire.
// Pure — no kube, no Secret reads — so it is fully table-testable. The CR is
// always named otelConfigCanonicalName and cluster-scoped (no namespace). Per W1
// the protocol inverse is lossy (wire "http" → the canonical http/protobuf; a CR
// authored as http/json round-trips to http/protobuf, documented & acceptable);
// per W2 the Secret key is re-materialized to the default since it never crosses
// the wire; per W3 the scalar sampling ratio maps back to a SamplingConfig.
func wireToCRD(wire OtelConfigWire) *ksquadv1.OTelConfig {
	return &ksquadv1.OTelConfig{
		ObjectMeta: metav1.ObjectMeta{Name: otelConfigCanonicalName},
		Spec: ksquadv1.OTelConfigSpec{
			Traces:  wireToSignal(wire.Spec.Traces),
			Metrics: wireToSignal(wire.Spec.Metrics),
			Logs:    wireToSignal(wire.Spec.Logs),
		},
	}
}

// wireToSignal maps one wire signal to its CRD SignalRouting; nil OR an empty
// endpoint ⇒ nil out (the signal stays unconfigured — the opt-in "no exporter"
// state, symmetric with signalToWire and the client's fromWire()).
func wireToSignal(w *signalWire) *ksquadv1.SignalRouting {
	if w == nil || w.Endpoint == "" {
		return nil
	}
	s := &ksquadv1.SignalRouting{
		Endpoint:           w.Endpoint,
		Protocol:           crdProtocol(w.Protocol),
		ResourceAttributes: w.ResourceAttributes,
		Sampling:           crdSampling(w.Sampling),
	}
	if ref := crdAuthRef(w.AuthSecretRef); ref != nil {
		s.Auth = ref
	}
	return s
}

// crdProtocol is the inverse of wireProtocol (W1): wire "grpc" → grpc; anything
// else (the wire's "http", or an absent value) → the canonical http/protobuf.
func crdProtocol(p string) ksquadv1.ExportProtocol {
	if p == "grpc" {
		return ksquadv1.ExportProtocolGRPC
	}
	return ksquadv1.ExportProtocolHTTPProtobuf
}

// crdAuthRef is the inverse of wireAuthRef (W2): "" ⇒ nil (no auth); "name" ⇒
// {Name}; "namespace/name" ⇒ {Namespace, Name}. The Secret KEY never crosses the
// wire, so it is re-materialized to defaultOTelAuthSecretKey (the CRD requires a
// non-empty key). A malformed ref (empty name after the slash) is left for the
// webhook/CEL to reject with a verbatim 4xx rather than silently repaired here.
func crdAuthRef(ref string) *ksquadv1.SecretKeyReference {
	if ref == "" {
		return nil
	}
	ns, name := "", ref
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		ns, name = ref[:i], ref[i+1:]
	}
	return &ksquadv1.SecretKeyReference{Name: name, Namespace: ns, Key: defaultOTelAuthSecretKey}
}

// crdSampling is the inverse of wireSampling (W3), preserving the pointer:
// nil ⇒ nil (sampling omitted); 0 ⇒ always_off; 1 ⇒ always_on; a ratio in (0,1) ⇒
// probabilistic with that ratio. (traces-only-sampling is the webhook's/CEL's rule
// to enforce, surfaced verbatim — this mapper does not silently drop it off a
// non-traces signal.)
func crdSampling(s *float64) *ksquadv1.SamplingConfig {
	if s == nil {
		return nil
	}
	switch {
	case *s <= 0:
		return &ksquadv1.SamplingConfig{Type: ksquadv1.SamplingTypeAlwaysOff}
	case *s >= 1:
		return &ksquadv1.SamplingConfig{Type: ksquadv1.SamplingTypeAlwaysOn}
	default:
		r := *s
		return &ksquadv1.SamplingConfig{Type: ksquadv1.SamplingTypeProbabilistic, Ratio: &r}
	}
}

// ── apply ─────────────────────────────────────────────────────────────────────

// otelConfigWriteResult is the response body for an apply. Cluster-scoped, so no
// namespace field (unlike composeResult).
type otelConfigWriteResult struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Revision  int    `json:"revision"`
	Operation string `json:"operation"` // "created" | "updated"
}

// otelWriteOutcome carries either a success (status 0, result set) or a
// single-status failure (status + msg) whose message is surfaced verbatim.
type otelWriteOutcome struct {
	result otelConfigWriteResult
	status int    // 0 ⇒ applied
	msg    string // failure message when status != 0
}

// apply upserts the canonical "default" CR: Get → present ⇒ a NEW revision via
// Update (compare-and-swap on resourceVersion, never an in-place snapshot
// mutation); absent ⇒ Create at revision 1. It maps webhook/CEL/apply errors onto
// the verbatim status the Console renders, and records a provenance row on success.
func (s *OTelConfigWriteService) apply(ctx context.Context, author discussion.AuthorContext, wire OtelConfigWire) otelWriteOutcome {
	desired := wireToCRD(wire)
	existing := &ksquadv1.OTelConfig{}
	getErr := s.applier.Get(ctx, client.ObjectKey{Name: otelConfigCanonicalName}, existing)

	var (
		rev       int
		operation = "created"
	)
	switch {
	case apierrors.IsNotFound(getErr):
		setRevision(desired, 1)
		if err := s.applier.Create(ctx, desired); err != nil {
			return otelApplyErrOutcome(err)
		}
		rev = 1
	case getErr != nil:
		return otelWriteOutcome{status: http.StatusBadGateway, msg: "otel-config read-before-write unavailable"}
	default:
		rev = readRevision(existing) + 1
		setRevision(desired, rev)
		// Carry the live resourceVersion so the Update is a compare-and-swap — a
		// concurrent change surfaces as a 409, exactly like the compose upsert.
		desired.SetResourceVersion(existing.GetResourceVersion())
		if err := s.applier.Update(ctx, desired); err != nil {
			return otelApplyErrOutcome(err)
		}
		operation = "updated"
	}

	s.recordProvenance(ctx, author.Principal, rev, operation)
	return otelWriteOutcome{result: otelConfigWriteResult{
		Kind: "OTelConfig", Name: otelConfigCanonicalName, Revision: rev, Operation: operation,
	}}
}

// otelApplyErrOutcome maps a Create/Update error onto the caller-facing status.
// A conflict is a friendly reload-and-retry 409; ANY other 4xx (a CRD CEL
// rejection ⇒ Invalid/422, an admission-webhook denial ⇒ Forbidden/403, etc.) is
// the caller's input error and is surfaced VERBATIM so the Console shows the exact
// reason; a 5xx or non-status error is infrastructure ⇒ 502.
func otelApplyErrOutcome(err error) otelWriteOutcome {
	if apierrors.IsConflict(err) {
		return otelWriteOutcome{status: http.StatusConflict, msg: "concurrent modification; reload and retry"}
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		if code := int(status.Status().Code); code >= 400 && code < 500 {
			return otelWriteOutcome{status: code, msg: err.Error()}
		}
	}
	return otelWriteOutcome{status: http.StatusBadGateway, msg: "otel-config apply failed"}
}

// recordProvenance appends the §6.5 apply event on a context DETACHED from the
// request (a client disconnect right after the apply must not cancel the row that
// records it), matching the compose/admin-mutation audit discipline.
func (s *OTelConfigWriteService) recordProvenance(ctx context.Context, principal string, rev int, op string) {
	if s.audit == nil {
		return
	}
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	s.audit(detached, "crd_applied", principal, map[string]any{
		"kind": "OTelConfig", "name": otelConfigCanonicalName, "revision": rev, "operation": op,
	})
}

// ── handler ─────────────────────────────────────────────────────────────────

// otelConfigWrite is the handler behind PUT/POST /api/otelconfig. It authenticates
// at the §13 choke point, gates on admin (OTelConfig is cluster-scoped ⇒
// platform-tier), decodes the OtelConfigWire body, and upserts the canonical CR.
// A validation/webhook rejection is surfaced verbatim (see otelApplyErrOutcome);
// the response is 201 on create, 200 on update.
func (s *Server) otelConfigWrite(writer *OTelConfigWriteService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		author, ok := discussion.AuthFromContext(r.Context())
		if !ok || author.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		if !author.IsAdmin {
			writeJSONError(w, http.StatusForbidden, "OTelConfig write is admin-only (cluster-scoped telemetry routing)")
			return
		}
		var wire OtelConfigWire
		if err := decodeJSON(w, r, &wire); err != nil {
			return // decodeJSON answered 400
		}
		out := writer.apply(r.Context(), author, wire)
		if out.status != 0 {
			writeJSONError(w, out.status, out.msg)
			return
		}
		code := http.StatusOK
		if out.result.Operation == "created" {
			code = http.StatusCreated
		}
		writeJSON(w, code, out.result)
		slog.InfoContext(r.Context(), "otel-config applied",
			"principal", author.Principal, "revision", out.result.Revision, "operation", out.result.Operation)
	}
}
