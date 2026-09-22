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
}

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
// surfaces this as a 200 with degraded=true (last-committed snapshot), not a 5xx.
var ErrWorkspaceBusy = errors.New("workspace busy: reader degraded to last-committed snapshot")

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
func (s *Server) projectFiles(reader WorkspaceReader) http.HandlerFunc {
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
			case errors.Is(err, ErrWorkspaceBusy), errors.Is(err, ErrNoBrowseTarget):
				// Busy / nothing-to-browse-yet are first-class degraded states, not
				// errors (AC7/S4c AC4): answer an empty listing with degraded=true.
				listing = &DirListing{Entries: []DirEntry{}, Degraded: true}
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
func (s *Server) projectFilesContent(reader WorkspaceReader) http.HandlerFunc {
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
			case errors.Is(err, ErrWorkspaceBusy), errors.Is(err, ErrNoBrowseTarget):
				// Surface busy / nothing-to-browse-yet as a degraded response with
				// empty data (S4c renders the banner).
				fc = &FileContent{Data: []byte{}, ContentType: "text", Degraded: true}
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
			// Data is base64-encoded by encoding/json for []byte fields.
			Data []byte `json:"data"`
		}{
			Size:        fc.Size,
			ContentType: fc.ContentType,
			Offset:      fc.Offset,
			Length:      fc.Length,
			Degraded:    fc.Degraded,
			Data:        fc.Data,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// projectFilesStat returns the handler for GET /api/projects/{projectId}/files/stat
// (ISI-4649). When reader is nil the handler answers 501. Git metadata is optional
// in the response — a nil Git field is the graceful no-git fallback, not an error.
func (s *Server) projectFilesStat(reader WorkspaceReader) http.HandlerFunc {
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
			case errors.Is(err, ErrWorkspaceBusy), errors.Is(err, ErrNoBrowseTarget):
				// Busy / nothing-to-browse-yet are first-class degraded states, not errors.
				st = &FileStat{Name: path.Base(cleanPath), Type: "file", Degraded: true}
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
