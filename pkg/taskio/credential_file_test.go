package taskio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestReadCredentialFromDirStages walks the credential file reader through its
// three observable states (ADR-0007): nothing landed (warm pod, pre-Bind),
// full set (handshake complete), and verifies the file↔struct inverse of
// SecretData including the optional trace-carrier files and the kubelet's
// trailing-newline behavior.
func TestReadCredentialFromDirStages(t *testing.T) {
	dir := t.TempDir()

	// Stage 1: empty mount — nothing landed, no error.
	if _, ok, err := ReadCredentialFromDir(dir); err != nil || ok {
		t.Fatalf("empty dir: ok=%v err=%v, want (false,nil)", ok, err)
	}

	// Stage 2: full set (with trailing newlines, as the projected-volume
	// writer emits) — the inverse of SecretData, byte-trimmed.
	cred := RunCredential{
		CoordURL:    "http://ksquad-apiserver.k8squad-system.svc/api/task-io",
		Token:       "hdr.payload.sig",
		WorkItemID:  "11111111-1111-1111-1111-111111111111",
		RunID:       "22222222-2222-2222-2222-222222222222",
		TraceParent: "00-traceid-spanid-01",
		TraceState:  "k=v",
	}
	for k, v := range cred.SecretData() {
		p := filepath.Join(dir, k)
		if err := os.WriteFile(p, append(v, '\n'), 0o644); err != nil { // kubelet appends the newline
			t.Fatalf("write %s: %v", k, err)
		}
	}
	got, ok, err := ReadCredentialFromDir(dir)
	if err != nil || !ok {
		t.Fatalf("full set: ok=%v err=%v, want (true,nil)", ok, err)
	}
	if got != cred {
		t.Fatalf("roundtrip mismatch:\n got %+v\nwant %+v", got, cred)
	}

	// Stage 3: a required file removed → not-yet-landed again (keep waiting),
	// never a partial credential.
	if err := os.Remove(filepath.Join(dir, EnvCoordToken)); err != nil {
		t.Fatalf("remove token: %v", err)
	}
	if _, ok, err := ReadCredentialFromDir(dir); err != nil || ok {
		t.Fatalf("missing token: ok=%v err=%v, want (false,nil)", ok, err)
	}
}

// TestReadCredentialFromDirEmptyFile treats a present-but-empty required file
// as not-yet-landed (mid-sync), not a corrupt credential.
func TestReadCredentialFromDirEmptyFile(t *testing.T) {
	dir := t.TempDir()
	for _, k := range RequiredCredentialFiles {
		if err := os.WriteFile(filepath.Join(dir, k), nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", k, err)
		}
	}
	if _, ok, err := ReadCredentialFromDir(dir); err != nil || ok {
		t.Fatalf("empty files: ok=%v err=%v, want (false,nil)", ok, err)
	}
}

// TestRequiredCredentialFilesPinned: the handshake's required set is exactly
// URL/token/work-item/run — the trace carrier stays optional (a carrier-less
// Bind context omits it honestly).
func TestRequiredCredentialFilesPinned(t *testing.T) {
	want := map[string]bool{EnvCoordURL: true, EnvCoordToken: true, EnvWorkItemID: true, EnvRunID: true}
	if len(RequiredCredentialFiles) != len(want) {
		t.Fatalf("required set = %v, want %v", RequiredCredentialFiles, want)
	}
	for _, k := range RequiredCredentialFiles {
		if !want[k] {
			t.Fatalf("unexpected required file %q", k)
		}
	}
}

// TestErrCredentialIncompleteExists pins the sentinel for future fsnotify
// callers that need to distinguish partial from absent.
func TestErrCredentialIncompleteExists(t *testing.T) {
	if !errors.Is(ErrCredentialIncomplete, ErrCredentialIncomplete) {
		t.Fatal("sentinel must be errors.Is-able")
	}
}
