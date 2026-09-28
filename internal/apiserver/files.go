package apiserver

// S4b — Project File Explorer read routes (ISI-3956/ISI-3991, ADR-0012 §D2).
//
// GET /api/projects/{projectId}/files          — list a directory
// GET /api/projects/{projectId}/files/content  — read a file (byte-range supported)
// GET /api/projects/{projectId}/files/stat     — file metadata + git last-change (ISI-4649)
//
// Both routes sit behind the §13 authz choke point and requireProjectRole(Viewer).
// A nil WorkspaceReader answers the documented 501 so S4c can render "not available
// yet" honestly. The reader-pod protocol (S4a) will satisfy the interface once wired;
// until then every request gets the 501 path.
//
// Tenancy: per-Project membership + global_role=admin short-circuit (rbac.go:56).
// There is NO fleetAdminTeam path (board rule ISI-3921/3925).
//
// Path safety: the route canonicalises and jail-checks every client-supplied path
// before delegating to the reader (defence-in-depth; S4a jails again inside the pod).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
)

// WorkspaceReader is the narrow read seam the files routes consume. S4a's reader-pod
// client will implement this; tests supply a map-backed stub.
//
// ListDir returns a paginated listing of the directory at dirPath (relative to the
// workspace jail root). ReadFile returns the content of filePath with optional
// byte-range [offset, offset+length). A zero length means "until EOF". The reader
// must cap content at a reasonable size ceiling and indicate binary files via the
// returned ContentType hint.
type WorkspaceReader interface {
	// ListDir returns the contents of dirPath inside the workspace jail.
	ListDir(ctx context.Context, projectID, dirPath string, page int) (*DirListing, error)

	// ReadFile returns content of filePath. If length == 0 the reader returns from
	// offset to the configured size cap. Callers that need byte ranges supply both.
	ReadFile(ctx context.Context, projectID, filePath string, offset, length int64) (*FileContent, error)

	// StatFile returns change metadata for filePath (ISI-4649): mtime always, plus
	// git last-change when the workspace is a git checkout. When git data is
	// unavailable the reader returns a FileStat with Git == nil — that is the
	// documented graceful fallback, not an error.
	StatFile(ctx context.Context, projectID, filePath string) (*FileStat, error)
}

// DirEntry is a single row in a directory listing.
type DirEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // "file" or "dir"
	Size int64  `json:"size"` // bytes; 0 for dirs
}

// DirListing is the paginated response body for ListDir.
type DirListing struct {
	Entries  []DirEntry `json:"entries"`
	NextPage int        `json:"nextPage,omitempty"` // 0 means last page
	// Degraded is true when the reader is returning a last-committed snapshot
	// because the live workspace PVC is busy (RWO held by a running agent).
	Degraded bool `json:"degraded,omitempty"`
	// Reason labels the degraded shape (ISI-5140): "workspace_busy" when snapshot
	// bytes are actually being served, "no_browse_target" for the honest
	// no-completed-run empty state. Empty when the listing is live.
	Reason string `json:"reason,omitempty"`
}

// Degraded-state reasons surfaced on DirListing / FileContent / FileStat (ISI-5140).
const (
	// reasonWorkspaceBusy labels reads served from the last-committed snapshot while
	// a running agent holds the workspace PVC.
	reasonWorkspaceBusy = "workspace_busy"
	// reasonNoBrowseTarget labels the honest empty state for a project with no
	// completed Run yet — NOT a snapshot, so the UI must not show the busy banner.
	reasonNoBrowseTarget = "no_browse_target"
)

// FileContent is the response body for ReadFile.
type FileContent struct {
	// Data holds the (possibly range-limited, size-capped) file bytes.
	Data []byte `json:"-"` // serialised raw, not as base64 JSON
	// Size is the total file size in bytes.
	Size int64 `json:"size"`
	// ContentType is "text" or "binary". Binary files must not be rendered as text.
	ContentType string `json:"contentType"`
	// Offset and Length describe which slice of the file Data covers.
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
	// Degraded mirrors DirListing.Degraded — snapshot vs live workspace.
	Degraded bool `json:"degraded,omitempty"`
	// Reason labels the degraded shape (ISI-5140); see DirListing.Reason.
	Reason string `json:"reason,omitempty"`
}

// FileStat is the response body for StatFile (ISI-4649).
type FileStat struct {
	Name string `json:"name"`
	Type string `json:"type"` // "file" or "dir"
	Size int64  `json:"size"` // bytes; 0 for dirs
	// ModTime is the filesystem mtime (RFC3339 on the wire).
	ModTime string `json:"modTime"`
	// Git is the last-change commit for this path when the workspace is a git
	// checkout; nil when git data is unavailable (graceful fallback).
	Git *GitChange `json:"git,omitempty"`
	// Degraded mirrors DirListing.Degraded — snapshot vs live workspace.
	Degraded bool `json:"degraded,omitempty"`
	// Reason labels the degraded shape (ISI-5140); see DirListing.Reason.
	Reason string `json:"reason,omitempty"`
}

// GitChange describes the most recent commit that touched a path.
type GitChange struct {
	CommitHash string `json:"commitHash"`
	Author     string `json:"author"`
	Message    string `json:"message"`
	// Timestamp is the commit time (RFC3339 on the wire).
	Timestamp string `json:"timestamp"`
}

// ErrWorkspaceBusy is returned by a WorkspaceReader when the workspace PVC is held
// by an active run on a node that the reader pod cannot co-schedule with. The route
// surfaces this as a 200 with degraded=true served from the LAST-COMMITTED SNAPSHOT
// (BusySnapshotReader, ISI-5140), never a fabricated empty listing.
var ErrWorkspaceBusy = errors.New("workspace busy: reader degraded to last-committed snapshot")

// BusySnapshotReader serves the last-committed workspace snapshot while a running
// agent holds the live PVC (ISI-5140). The route consults it on ErrWorkspaceBusy;
// when it is nil (or itself errors / reports no snapshot) the busy branch degrades
// to an empty listing with reason="workspace_busy" so S4c can render an honest
// "snapshot unavailable" state instead of impersonating an empty workspace.
type BusySnapshotReader interface {
	// ListSnapshotDir returns the snapshot listing for dirPath relative to the
	// snapshot jail root, or ErrNoWorkspaceSnapshot when no snapshot exists.
	ListSnapshotDir(ctx context.Context, projectID, dirPath string, page int) (*DirListing, error)
	// ReadSnapshotFile returns snapshot file content with the same range contract
	// as WorkspaceReader.ReadFile, or ErrNoWorkspaceSnapshot.
	ReadSnapshotFile(ctx context.Context, projectID, filePath string, offset, length int64) (*FileContent, error)
	// StatSnapshotFile returns snapshot change metadata, or ErrNoWorkspaceSnapshot.
	StatSnapshotFile(ctx context.Context, projectID, filePath string) (*FileStat, error)
}

// ErrNoWorkspaceSnapshot is returned by a BusySnapshotReader when the project has
// no servable last-committed snapshot (capture truncated / blob store not wired,
// pre-ISI-2900). The routes surface it as the honest degraded empty form.
var ErrNoWorkspaceSnapshot = errors.New("no last-committed workspace snapshot available")

// workspaceJailPath canonicalises a client-supplied path and verifies it stays inside
// the workspace jail (equivalent to the pod-internal jail in S4a, AC3). It returns
// the cleaned relative path, or an error if the path escapes.
//
// Rules:
//   - Strip leading slashes so "absolute" paths become relative.
//   - path.Clean resolves all . and .. segments.
//   - If the result starts with ".." the path escapes the root — rejected.
//   - Empty path (or bare "/") resolves to "." (the jail root) for directory listing.
func workspaceJailPath(raw string) (string, error) {
	// Normalise: trim surrounding whitespace and leading slashes.
	p := strings.TrimSpace(raw)
	p = strings.TrimLeft(p, "/")
	if p == "" {
		return ".", nil
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path escapes workspace root")
	}
	return clean, nil
}

// projectFiles returns the handler for GET /api/projects/{projectId}/files.
// When reader is nil the handler answers 501 (not-implemented convention, server.go:800).
// busy (may be nil) is the last-committed-snapshot seam consulted on ErrWorkspaceBusy (ISI-5140).
func (s *Server) projectFiles(reader WorkspaceReader, busy BusySnapshotReader) http.HandlerFunc {
	if reader == nil {
		return notImplemented("project file-explorer list", "ISI-3991: wire a WorkspaceReader (S4a reader-pod client) to enable")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := decodePathVar(mux.Vars(r)["projectId"])

		rawPath := r.URL.Query().Get("path")
		cleanPath, err := workspaceJailPath(rawPath)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid path: "+err.Error())
			return
		}

		page := 0
		if pStr := r.URL.Query().Get("page"); pStr != "" {
			if _, err := parseIntParam(pStr, &page); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid page parameter")
				return
			}
		}

		listing, err := reader.ListDir(r.Context(), projectID, cleanPath, page)
		if err != nil {
			switch {
			case errors.Is(err, ErrProjectNotFound):
				// Unknown projectId: existence-hiding 404, same as the dashboard spine.
				writeJSONError(w, http.StatusNotFound, "project not found")
				return
			case errors.Is(err, ErrWorkspaceBusy):
				// ISI-5140: busy is a first-class degraded state (AC7) — serve the
				// LAST-COMMITTED SNAPSHOT when one exists, never a fabricated empty
				// listing. Without a snapshot the empty form is honestly labelled.
				var snapErr error
				listing, snapErr = busyList(r.Context(), busy, projectID, cleanPath, page)
				if snapErr != nil {
					listing = &DirListing{Entries: []DirEntry{}, Degraded: true, Reason: reasonWorkspaceBusy}
				} else {
					listing.Degraded = true
					listing.Reason = reasonWorkspaceBusy
				}
			case errors.Is(err, ErrNoBrowseTarget):
				// Nothing-to-browse-yet is an honest empty state, NOT a snapshot:
				// degraded=false + reason so S4c renders "no completed run yet"
				// without the busy banner (ISI-5140).
				listing = &DirListing{Entries: []DirEntry{}, Reason: reasonNoBrowseTarget}
			default:
				writeJSONError(w, http.StatusInternalServerError, "workspace read error")
				return
			}
		}

		writeJSON(w, http.StatusOK, listing)
	}
}

// projectFilesContent returns the handler for GET /api/projects/{projectId}/files/content.
// When reader is nil the handler answers 501.
func (s *Server) projectFilesContent(reader WorkspaceReader, busy BusySnapshotReader) http.HandlerFunc {
	if reader == nil {
		return notImplemented("project file-explorer content", "ISI-3991: wire a WorkspaceReader (S4a reader-pod client) to enable")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := decodePathVar(mux.Vars(r)["projectId"])

		rawPath := r.URL.Query().Get("path")
		if rawPath == "" {
			writeJSONError(w, http.StatusBadRequest, "path query param required")
			return
		}
		cleanPath, err := workspaceJailPath(rawPath)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid path: "+err.Error())
			return
		}
		// Reject jail root as a file read target.
		if cleanPath == "." {
			writeJSONError(w, http.StatusBadRequest, "path must name a file, not the workspace root")
			return
		}

		var offset, length int64
		if oStr := r.URL.Query().Get("offset"); oStr != "" {
			if _, err := parseInt64Param(oStr, &offset); err != nil || offset < 0 {
				writeJSONError(w, http.StatusBadRequest, "invalid offset parameter")
				return
			}
		}
		if lStr := r.URL.Query().Get("length"); lStr != "" {
			if _, err := parseInt64Param(lStr, &length); err != nil || length < 0 {
				writeJSONError(w, http.StatusBadRequest, "invalid length parameter")
				return
			}
		}

		fc, err := reader.ReadFile(r.Context(), projectID, cleanPath, offset, length)
		if err != nil {
			switch {
			case errors.Is(err, ErrProjectNotFound):
				writeJSONError(w, http.StatusNotFound, "project not found")
				return
			case errors.Is(err, ErrWorkspaceBusy):
				// ISI-5140: serve the last-committed snapshot bytes when available.
				var snapErr error
				fc, snapErr = busyRead(r.Context(), busy, projectID, cleanPath, offset, length)
				if snapErr != nil {
					fc = &FileContent{Data: []byte{}, ContentType: "text", Degraded: true, Reason: reasonWorkspaceBusy}
				} else {
					fc.Degraded = true
					fc.Reason = reasonWorkspaceBusy
				}
			case errors.Is(err, ErrNoBrowseTarget):
				// Honest no-completed-run empty state — no snapshot, no banner.
				fc = &FileContent{Data: []byte{}, ContentType: "text", Reason: reasonNoBrowseTarget}
			default:
				writeJSONError(w, http.StatusInternalServerError, "workspace read error")
				return
			}
		}

		// Write a multipart-ish response: JSON envelope then raw bytes.
		// For binary files the client must not attempt UTF-8 decoding.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		resp := struct {
			Size        int64  `json:"size"`
			ContentType string `json:"contentType"`
			Offset      int64  `json:"offset"`
			Length      int64  `json:"length"`
			Degraded    bool   `json:"degraded,omitempty"`
			Reason      string `json:"reason,omitempty"`
			// Data is base64-encoded by encoding/json for []byte fields.
			Data []byte `json:"data"`
		}{
			Size:        fc.Size,
			ContentType: fc.ContentType,
			Offset:      fc.Offset,
			Length:      fc.Length,
			Degraded:    fc.Degraded,
			Reason:      fc.Reason,
			Data:        fc.Data,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// projectFilesStat returns the handler for GET /api/projects/{projectId}/files/stat
// (ISI-4649). When reader is nil the handler answers 501. Git metadata is optional
// in the response — a nil Git field is the graceful no-git fallback, not an error.
func (s *Server) projectFilesStat(reader WorkspaceReader, busy BusySnapshotReader) http.HandlerFunc {
	if reader == nil {
		return notImplemented("project file-explorer stat", "ISI-4649: wire a WorkspaceReader (S4a reader-pod client) to enable")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := decodePathVar(mux.Vars(r)["projectId"])

		rawPath := r.URL.Query().Get("path")
		if rawPath == "" {
			writeJSONError(w, http.StatusBadRequest, "path query param required")
			return
		}
		cleanPath, err := workspaceJailPath(rawPath)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid path: "+err.Error())
			return
		}
		// Stat on the jail root is meaningless for change metadata.
		if cleanPath == "." {
			writeJSONError(w, http.StatusBadRequest, "path must name a file, not the workspace root")
			return
		}

		st, err := reader.StatFile(r.Context(), projectID, cleanPath)
		if err != nil {
			switch {
			case errors.Is(err, ErrProjectNotFound):
				writeJSONError(w, http.StatusNotFound, "project not found")
				return
			case errors.Is(err, ErrWorkspaceBusy):
				// ISI-5140: stat from the last-committed snapshot when available.
				var snapErr error
				st, snapErr = busyStat(r.Context(), busy, projectID, cleanPath)
				if snapErr != nil {
					st = &FileStat{Name: path.Base(cleanPath), Type: "file", Degraded: true, Reason: reasonWorkspaceBusy}
				} else {
					st.Degraded = true
					st.Reason = reasonWorkspaceBusy
				}
			case errors.Is(err, ErrNoBrowseTarget):
				// Honest no-completed-run empty state — no snapshot, no banner.
				st = &FileStat{Name: path.Base(cleanPath), Type: "file", Reason: reasonNoBrowseTarget}
			default:
				writeJSONError(w, http.StatusInternalServerError, "workspace read error")
				return
			}
		}

		writeJSON(w, http.StatusOK, st)
	}
}

// parseIntParam parses a decimal string into *dst.
func parseIntParam(s string, dst *int) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	*dst = v
	return v, nil
}

// parseInt64Param parses a decimal string into *dst.
func parseInt64Param(s string, dst *int64) (int64, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	*dst = v
	return v, nil
}

// busyList serves the last-committed snapshot listing for a busy workspace (ISI-5140).
// A nil BusySnapshotReader or ErrNoWorkspaceSnapshot yields ErrNoWorkspaceSnapshot so
// the route can label the empty form honestly.
func busyList(ctx context.Context, busy BusySnapshotReader, projectID, dirPath string, page int) (*DirListing, error) {
	if busy == nil {
		return nil, ErrNoWorkspaceSnapshot
	}
	return busy.ListSnapshotDir(ctx, projectID, dirPath, page)
}

// busyRead serves snapshot file content for a busy workspace (ISI-5140).
func busyRead(ctx context.Context, busy BusySnapshotReader, projectID, filePath string, offset, length int64) (*FileContent, error) {
	if busy == nil {
		return nil, ErrNoWorkspaceSnapshot
	}
	return busy.ReadSnapshotFile(ctx, projectID, filePath, offset, length)
}

// busyStat serves snapshot change metadata for a busy workspace (ISI-5140).
func busyStat(ctx context.Context, busy BusySnapshotReader, projectID, filePath string) (*FileStat, error) {
	if busy == nil {
		return nil, ErrNoWorkspaceSnapshot
	}
	return busy.StatSnapshotFile(ctx, projectID, filePath)
}
