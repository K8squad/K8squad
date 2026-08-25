package coord

import (
	"errors"
	"strings"
	"testing"

	"github.com/K8squad/K8squad/pkg/reconcile"
)

// DB-free unit tests for the pure-logic helpers in pkg/coord (ISI-3213, child of
// ISI-2714). These functions carry no Postgres dependency — parsing, key
// validation, provenance/step classification — so they run in the ci.yml unit
// lane and lift the gated coverage number.

func TestIsCancelTerminalStep(t *testing.T) {
	terminal := []string{"succeeded", "failed", "cancelled"}
	for _, s := range terminal {
		if !isCancelTerminalStep(s) {
			t.Errorf("isCancelTerminalStep(%q) = false, want true", s)
		}
	}
	nonTerminal := []string{"", "claimed", "dispatched", "running", "SUCCEEDED", "Cancelled", "cancel"}
	for _, s := range nonTerminal {
		if isCancelTerminalStep(s) {
			t.Errorf("isCancelTerminalStep(%q) = true, want false", s)
		}
	}
}

func TestValidateCredentialKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"empty is valid opaque identity", "", false},
		{"simple identity", "anthropic-team-a", false},
		{"max length exactly 253", strings.Repeat("a", 253), false},
		{"over max length looks like token material", strings.Repeat("a", 254), true},
		{"embedded space", "team a", true},
		{"leading control char", "\x01team", true},
		{"tab is control", "team\tb", true},
		{"del char 0x7f", "team\x7f", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCredentialKey("credentialKey", tt.key)
			if tt.wantErr && err == nil {
				t.Fatalf("validateCredentialKey(%q) = nil, want error", tt.key)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateCredentialKey(%q) = %v, want nil", tt.key, err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "credentialKey") {
				t.Errorf("error %q does not name the field", err.Error())
			}
		})
	}
}

func TestArtifactKind(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"run-1/patch", "patch"},
		{"run-1/logs/stdout", "stdout"},
		{"patch", "patch"},         // unslashed → whole key
		{"", ""},                   // empty
		{"trailing/", "trailing/"}, // trailing slash: no segment after → whole key
		{"a/b/c/d", "d"},
	}
	for _, tt := range tests {
		if got := artifactKind(tt.key); got != tt.want {
			t.Errorf("artifactKind(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

func TestProdEffectsBoundTaskID(t *testing.T) {
	const run = "run_uuid"
	e := &ProdEffects{runID: run}

	// The machine's fixture run key maps back to the authoritative bound runID.
	if got := e.boundTaskID(reconcile.RunID); got != run {
		t.Errorf("boundTaskID(RunID) = %q, want %q", got, run)
	}
	// A suffixed fixture key (e.g. lap markers) keeps its suffix on the real run.
	if got := e.boundTaskID(reconcile.RunID + "#lap2"); got != run+"#lap2" {
		t.Errorf("boundTaskID(RunID+suffix) = %q, want %q", got, run+"#lap2")
	}
	// An unrelated task id is namespaced under the run.
	if got := e.boundTaskID("other"); got != run+"/other" {
		t.Errorf("boundTaskID(other) = %q, want %q", got, run+"/other")
	}
}

func TestProdEffectsInitiator(t *testing.T) {
	if got := (&ProdEffects{}).initiator(); got != nil {
		t.Errorf("initiator() with empty initiatedBy = %v, want nil", got)
	}
	if got := (&ProdEffects{initiatedBy: "user-42"}).initiator(); got != "user-42" {
		t.Errorf("initiator() = %v, want user-42", got)
	}
}

func TestProdEffectsErrIsSticky(t *testing.T) {
	e := &ProdEffects{}
	if e.Err() != nil {
		t.Fatalf("fresh ProdEffects.Err() = %v, want nil", e.Err())
	}
	first := errors.New("first")
	second := errors.New("second")
	e.fail(first)
	e.fail(second) // sticky: the first error wins
	if !errors.Is(e.Err(), first) {
		t.Errorf("Err() = %v, want first (sticky)", e.Err())
	}
}
