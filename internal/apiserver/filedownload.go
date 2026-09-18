package apiserver

// S4b — Project File Explorer download route (ISI-4650, ADR-0012 §D2).
//
// GET /api/projects/{projectId}/files/download?path=<p>
//
//   - <p> names a file      → streamed as an attachment (Content-Disposition) at
//     application/octet-stream, capped at maxDownloadFileBytes.
//   - <p> names a directory → a server-built tar.gz archive of the subtree, streamed
//     with aggregate caps (maxArchiveEntries / maxArchiveTotalBytes) enforced BEFORE
//     the first byte is written, so an over-cap tree answers 413 rather than a
//     silently truncated archive.
//
// The route rides the SAME §13 authz choke point + requireProjectRole(Viewer) as the
// list/content routes (server.go), reuses workspaceJailPath for the path jail, and a
// nil WorkspaceReader answers the documented 501 (honest "not available yet").
//
// File-vs-directory discrimination: the route attempts ListDir first; a reader
// answers ErrNotDirectory for a path that names a regular file (the S4a readserver
// already answers 400 "not a directory" — mapped in readerpodreader.go mapReadErr).
// ErrWorkspaceBusy surfaces as 503 (a download has no honest degraded form).

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
)

const (
	// maxDownloadFileBytes caps a single-file download (and any single file inside an
	// archive). 64 MiB is generous for workspace artifacts while bounding memory.
	maxDownloadFileBytes = 64 << 20
	// maxArchiveTotalBytes caps the aggregate uncompressed size of a folder archive.
	maxArchiveTotalBytes = 256 << 20
	// maxArchiveEntries caps the number of files in a folder archive (walk-bomb guard).
	maxArchiveEntries = 2000
)

// ErrNotDirectory is returned by a WorkspaceReader.ListDir when the path names a
// regular file. The download route uses it to fall back to the file path.
var ErrNotDirectory = errors.New("workspace path is not a directory")

// errArchiveTooLarge / errArchiveTooManyFiles / errArchiveFileTooLarge are the
// pre-stream cap violations; the route maps all three to 413.
var (
	errArchiveTooLarge     = errors.New("archive exceeds aggregate size cap")
	errArchiveTooManyFiles = errors.New("archive exceeds file-count cap")
	errArchiveFileTooLarge = errors.New("file exceeds single-file download cap")
)

// archiveEntry is one file queued for the archive stream.
type archiveEntry struct {
	relPath string // jail-relative path passed to ReadFile
	size    int64  // size reported by ListDir (phase-1 cap accounting)
}

// projectFilesDownload returns the handler for GET /api/projects/{projectId}/files/download.
func (s *Server) projectFilesDownload(reader WorkspaceReader) http.HandlerFunc {
	if reader == nil {
		return notImplemented("project file-explorer download", "ISI-4650: wire a WorkspaceReader (S4a reader-pod client) to enable")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := mux.Vars(r)["projectId"]

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

		// File or directory? ListDir answers for directories; ErrNotDirectory flips to file.
		_, listErr := reader.ListDir(r.Context(), projectID, cleanPath, 0)
		switch {
		case listErr == nil:
			s.streamArchive(w, r, reader, projectID, cleanPath)
		case errors.Is(listErr, ErrNotDirectory):
			s.streamFile(w, r, reader, projectID, cleanPath)
		case errors.Is(listErr, ErrWorkspaceBusy):
			writeJSONError(w, http.StatusServiceUnavailable, "workspace busy: download unavailable")
		default:
			writeJSONError(w, http.StatusInternalServerError, "workspace read error")
		}
	}
}

// streamFile serves a single file as an attachment, capped at maxDownloadFileBytes.
func (s *Server) streamFile(w http.ResponseWriter, r *http.Request, reader WorkspaceReader, projectID, cleanPath string) {
	fc, err := reader.ReadFile(r.Context(), projectID, cleanPath, 0, maxDownloadFileBytes)
	if err != nil {
		if errors.Is(err, ErrWorkspaceBusy) {
			writeJSONError(w, http.StatusServiceUnavailable, "workspace busy: download unavailable")
		} else {
			writeJSONError(w, http.StatusInternalServerError, "workspace read error")
		}
		return
	}
	if fc.Size > maxDownloadFileBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge,
			"file exceeds download cap of "+strconv.FormatInt(maxDownloadFileBytes, 10)+" bytes")
		return
	}
	name := sanitizeDownloadName(path.Base(cleanPath))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Content-Length", strconv.Itoa(len(fc.Data)))
	// Defence in depth against MIME sniffing of attacker-controlled workspace bytes.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	// #nosec G705 -- the payload is written with an explicit application/octet-stream
	// Content-Type and attachment Content-Disposition (set above) plus nosniff, so the
	// browser downloads it and never renders it as active content. Filename is sanitised.
	_, _ = w.Write(fc.Data)
}

// streamArchive builds a tar.gz of the subtree at cleanPath. Phase 1 walks the tree
// (paginated ListDir) collecting files and enforcing caps; only after the walk passes
// does phase 2 stream, so caps never truncate a partially-written archive.
func (s *Server) streamArchive(w http.ResponseWriter, r *http.Request, reader WorkspaceReader, projectID, cleanPath string) {
	files, err := collectArchiveFiles(r.Context(), reader, projectID, cleanPath)
	if err != nil {
		switch {
		case errors.Is(err, errArchiveTooLarge),
			errors.Is(err, errArchiveTooManyFiles),
			errors.Is(err, errArchiveFileTooLarge):
			writeJSONError(w, http.StatusRequestEntityTooLarge, err.Error())
		case errors.Is(err, ErrWorkspaceBusy):
			writeJSONError(w, http.StatusServiceUnavailable, "workspace busy: download unavailable")
		default:
			writeJSONError(w, http.StatusInternalServerError, "workspace read error")
		}
		return
	}

	base := sanitizeDownloadName(path.Base(cleanPath))
	if cleanPath == "." {
		base = "workspace"
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", base+".tar.gz"))
	w.WriteHeader(http.StatusOK)

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		fc, err := reader.ReadFile(r.Context(), projectID, f.relPath, 0, maxDownloadFileBytes)
		if err != nil {
			// Mid-stream failure: the response is already committed; aborting closes the
			// stream so the client sees a truncated archive rather than a corrupt entry.
			return
		}
		rel := f.relPath
		if cleanPath != "." {
			rel = strings.TrimPrefix(rel, cleanPath+"/")
		}
		hdr := &tar.Header{
			Name: path.Join(base, rel),
			Mode: 0o644,
			Size: int64(len(fc.Data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return
		}
		if _, err := tw.Write(fc.Data); err != nil {
			return
		}
	}
	_ = tw.Close()
	_ = gz.Close()
}

// collectArchiveFiles walks the subtree at root via paginated ListDir, returning every
// file with its jail-relative path. Caps are enforced during the walk.
func collectArchiveFiles(ctx context.Context, reader WorkspaceReader, projectID, root string) ([]archiveEntry, error) {
	var files []archiveEntry
	var total int64
	stack := []string{root}
	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		page := 0
		for {
			listing, err := reader.ListDir(ctx, projectID, dir, page)
			if err != nil {
				return nil, err
			}
			for _, e := range listing.Entries {
				p := path.Join(dir, e.Name)
				if e.Type == "dir" {
					stack = append(stack, p)
					continue
				}
				if e.Size > maxDownloadFileBytes {
					return nil, errArchiveFileTooLarge
				}
				files = append(files, archiveEntry{relPath: p, size: e.Size})
				total += e.Size
				if len(files) > maxArchiveEntries {
					return nil, errArchiveTooManyFiles
				}
				if total > maxArchiveTotalBytes {
					return nil, errArchiveTooLarge
				}
			}
			if listing.NextPage == 0 {
				break
			}
			page = listing.NextPage
		}
	}
	return files, nil
}

// sanitizeDownloadName strips characters that would break the quoted-string form of
// Content-Disposition (quotes, backslashes, control chars). An empty result (or the
// jail-root sentinel) falls back to "download".
func sanitizeDownloadName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." {
		return "download"
	}
	return name
}
