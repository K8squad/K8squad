package taskio

// Env var names for the W3C trace carrier that joins a task subprocess's spans
// onto its Run's trace. They travel with the task-io bootstrap set (they are the
// same names telemetry.Inject's carrier keys render to) so a RunCredential can be
// rendered to a single carrier without the mint site re-deriving them.
const (
	EnvTraceParent = "TRACEPARENT"
	EnvTraceState  = "TRACESTATE"
)

// CoordMountPath is the in-pod directory where the warmpool/sandbox topology
// (topology 2, ADR-0007 channel A) mounts the per-sandbox Secret that carries a
// RunCredential. The Bind-path writer names the Secret after the sandbox_ref
// (== pod name) and renders the credential to its keys via SecretData; the
// sandbox supervisor reads each value from a file under this path (the env→path
// contract), e.g. <CoordMountPath>/KSQUAD_COORD_TOKEN. Unlike the shim's env
// carrier, this survives the Boot-before-Bind ordering: the volume mounts
// optionally at Boot and the file appears once the operator writes the Secret at
// Bind.
const CoordMountPath = "/var/run/ksquad/coord"

// CoordVolumeName is the pod volume / volumeMount name for the projected
// per-sandbox task-io Secret (CoordMountPath).
const CoordVolumeName = "ksquad-coord-taskio"

// RunCredential is the carrier-agnostic content of the run-scoped task-io
// credential — the exact set an agent subprocess needs to talk back to the coord
// API for its OWN run (re-read task, comment, update status, checkout), plus the
// W3C trace carrier. It is minted once, from the operator/rundrive reconcile layer
// that owns the *api.Run (so deriveRunScopes + agentName resolve the ISI-3626 role
// scopes and the principal), then rendered to whichever carrier the dispatch
// topology uses:
//
//   - operator-spawned shim (topology 1): rendered to env via EnvKV — the live v1
//     path in pkg/controller/rundrive.
//   - warmpool/sandbox Bind (topology 2, ADR-0007): rendered to projected Secret
//     keys via SecretData — the token is minted at Bind (when RUN_ID/WORK_ITEM_ID
//     exist) and written to a per-sandbox Secret the kubelet propagates into the
//     already-running pod at /var/run/ksquad/coord.
//
// The content is identical across carriers by construction, so the two topologies
// cannot drift — only the carrier differs. RunCredential holds NO operator secret
// (only the run-scoped token + trace carrier + IDs), so rendering it to either
// carrier preserves the minimal-env invariant.
type RunCredential struct {
	CoordURL    string // KSQUAD_COORD_URL — in-cluster coord/apiserver base URL
	Token       string // KSQUAD_COORD_TOKEN — run-scoped, minted via Minter.MintWithScopes
	WorkItemID  string // WORK_ITEM_ID — the bound work item (also bound in the token)
	RunID       string // RUN_ID — the bound Run uid (also bound in the token)
	TraceParent string // TRACEPARENT — W3C carrier (telemetry.Inject "traceparent")
	TraceState  string // TRACESTATE — W3C carrier (telemetry.Inject "tracestate")
}

// EnvKV renders the credential to a slice of NAME=value env entries, one per
// non-empty field, in a stable order (coord URL, token, work item, run, then the
// trace carrier). Empty fields are omitted so a credential missing an optional
// piece never emits a bare NAME= entry. This is the env carrier (topology 1).
func (c RunCredential) EnvKV() []string {
	var kv []string
	for _, p := range c.pairs() {
		kv = append(kv, p.k+"="+p.v)
	}
	return kv
}

// SecretData renders the credential to a map of Secret keys (topology 2, the
// projected Secret volume). Keys match EnvKV's names exactly and values are the
// same bytes, so the file an in-pod supervisor reads carries byte-identical
// content to the env a shim subprocess reads. Empty fields are omitted. Returns a
// non-nil map (possibly empty) so callers can range/assign without a nil check.
func (c RunCredential) SecretData() map[string][]byte {
	data := make(map[string][]byte, 6)
	for _, p := range c.pairs() {
		data[p.k] = []byte(p.v)
	}
	return data
}

type kvPair struct{ k, v string }

// pairs is the single source of the credential's key/value content, so EnvKV and
// SecretData cannot drift in field set or ordering.
func (c RunCredential) pairs() []kvPair {
	var out []kvPair
	for _, p := range []kvPair{
		{EnvCoordURL, c.CoordURL},
		{EnvCoordToken, c.Token},
		{EnvWorkItemID, c.WorkItemID},
		{EnvRunID, c.RunID},
		{EnvTraceParent, c.TraceParent},
		{EnvTraceState, c.TraceState},
	} {
		if p.v != "" {
			out = append(out, p)
		}
	}
	return out
}
