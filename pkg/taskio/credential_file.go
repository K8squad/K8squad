package taskio

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrCredentialIncomplete is returned by ReadCredentialFromDir when some (but
// not all) of the credential files have landed in the mount — the observable
// mid-propagation state the in-pod supervisor polls through (ADR-0007: the
// kubelet syncs the projected Secret's files independently; the supervisor
// waits rather than racing them).
var ErrCredentialIncomplete = errors.New("taskio: credential files incomplete")

// RequiredCredentialFiles are the Secret keys that MUST be present under
// CoordMountPath before the supervisor considers the Bind→pod handshake
// complete. The trace-carrier files are optional (a carrier-less Bind context
// omits them honestly), but URL/token/IDs are the handshake's substance.
var RequiredCredentialFiles = []string{
	EnvCoordURL,
	EnvCoordToken,
	EnvWorkItemID,
	EnvRunID,
}

// ReadCredentialFromDir reads a RunCredential from the files under dir — the
// in-pod side of the ADR-0007 channel-A contract. The projected Secret volume
// materializes one file per key under CoordMountPath; this is the file↔struct
// inverse of SecretData, so the supervisor reconstructs byte-identical content
// to what the Bind-path writer minted.
//
// ok=false with nil error means nothing has landed yet (warm pod, pre-Bind).
// ErrCredentialIncomplete means a partial set is visible (mid-sync); callers
// that poll should treat both as "keep waiting". Trace-carrier files are
// optional and only read when present.
func ReadCredentialFromDir(dir string) (cred RunCredential, ok bool, err error) {
	for _, name := range RequiredCredentialFiles {
		v, rerr := readTrimmed(filepath.Join(dir, name))
		if rerr != nil {
			if os.IsNotExist(rerr) {
				return RunCredential{}, false, nil
			}
			return RunCredential{}, false, rerr
		}
		if v == "" {
			// The file exists but empty: treat as not-yet-landed rather than a
			// corrupt credential — the kubelet may still be syncing content.
			return RunCredential{}, false, nil
		}
		switch name {
		case EnvCoordURL:
			cred.CoordURL = v
		case EnvCoordToken:
			cred.Token = v
		case EnvWorkItemID:
			cred.WorkItemID = v
		case EnvRunID:
			cred.RunID = v
		}
	}
	// Optional trace-carrier files.
	if v, rerr := readTrimmed(filepath.Join(dir, EnvTraceParent)); rerr == nil && v != "" {
		cred.TraceParent = v
	}
	if v, rerr := readTrimmed(filepath.Join(dir, EnvTraceState)); rerr == nil && v != "" {
		cred.TraceState = v
	}
	return cred, true, nil
}

// readTrimmed reads a credential file and trims the trailing newline the
// kubelet's projected-volume writer appends to each Secret key's bytes.
func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- path is joined from the fixed CoordMountPath + constant key names
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
