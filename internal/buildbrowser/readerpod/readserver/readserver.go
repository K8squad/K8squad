/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package readserver is the in-pod, read-only list/read HTTP protocol served INSIDE the story 8.7f
// reader pod (ISI-2905, ISI-4005 / S4a of ISI-3956). It is the epic's largest trust boundary, so it
// is deliberately isolated from the launcher: it carries ZERO Kubernetes-client dependencies and only
// ever READS a filesystem jail rooted at the ReadOnly-mounted Project PVC (/workspace).
//
// Contract (ADR-0012 §"net-new work" 2; pinned by S4a task 1):
//
//	GET /list?path=<rel>&page=<n>          → 200 DirListing{entries[{name,type,size}], nextPage}
//	GET /read?path=<rel>&offset=<o>&length=<l> → 200 FileContent{size,contentType,offset,length,data}
//	GET /healthz                            → 200
//
// The apiserver (S4b) reaches this ONLY over an in-cluster ClusterIP HTTP call — there is NO
// pods/exec, NO kubectl cp, NO apiserver PVC mount (all rejected in ADR-0012 §Decision). There are
// deliberately NO write/delete/rename/exec verbs: the surface is list + read only.
//
// Containment invariants (AC3/AC4):
//   - Every client path is filepath.Clean-ed, leading slashes stripped (so "absolute" paths become
//     jail-relative), and a resulting ".." escape is rejected before any filesystem access.
//   - The resolved real path (filepath.EvalSymlinks) MUST stay under the resolved real jail root, so a
//     symlink that points outside /workspace is rejected, never followed.
//   - Reads are byte-capped and byte-range aware, so a large file can never stream unbounded into the
//     apiserver; binary content is flagged so it is not later rendered as text.
package readserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DefaultRoot is the ReadOnly PVC mount path the reader pod jails under. It is stable by contract:
// S4-prereq and agent pods assume /workspace (S4a technical notes).
const DefaultRoot = "/workspace"

// Protocol bounds. maxReadBytes caps a single read response so a directory/file response is never an
// unbounded stream into the apiserver (AC4); defaultPageSize bounds a listing page (AC3).
const (
	maxReadBytes    = int64(1 << 20) // 1 MiB per read response (byte-range pages larger files)
	defaultPageSize = 1000           // directory entries per listing page
	sniffLen        = 512            // bytes inspected for binary detection
)

var (
	// errEscape is a path that resolves outside the jail root (traversal or symlink escape).
	errEscape = errors.New("path escapes workspace root")
	// errNotFound is a path that does not exist inside the jail.
	errNotFound = errors.New("path not found")
)

// Entry is a single row in a directory listing (mirrors apiserver DirEntry wire shape).
type Entry struct {
	Name string `json:"name"`
	Type string `json:"type"` // "file" or "dir"
	Size int64  `json:"size"` // bytes; 0 for dirs
}

// DirListing is the /list response body (mirrors apiserver DirListing wire shape).
type DirListing struct {
	Entries  []Entry `json:"entries"`
	NextPage int     `json:"nextPage,omitempty"` // 0 ⇒ last page
}

// FileContent is the /read response body (mirrors apiserver FileContent wire shape). Data is
// base64-encoded by encoding/json for []byte; the apiserver client decodes and re-frames it.
type FileContent struct {
	Size        int64  `json:"size"`        // total file size in bytes
	ContentType string `json:"contentType"` // "text" or "binary"
	Offset      int64  `json:"offset"`      // start of the returned slice
	Length      int64  `json:"length"`      // len(Data)
	Data        []byte `json:"data"`        // the (range-limited, size-capped) bytes
}

// Server serves the jailed RO list/read protocol over an already-resolved real jail root.
type Server struct {
	realRoot string // filepath.EvalSymlinks(root): the canonical jail root every path must stay under
	maxBytes int64
	pageSize int
}

// New builds a Server jailed at root. It resolves symlinks in root once (so the jail check compares
// canonical paths) and fails closed if root is missing or is not a directory.
func New(root string) (*Server, error) {
	if root == "" {
		root = DefaultRoot
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, errors.New("readserver: root is not a directory: " + root)
	}
	return &Server{realRoot: real, maxBytes: maxReadBytes, pageSize: defaultPageSize}, nil
}

// Handler returns the read-only protocol mux. Only GET is served; there are no mutating verbs.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/list", s.handleList)
	mux.HandleFunc("/read", s.handleRead)
	return mux
}

// jail canonicalises a client-supplied path and verifies it stays inside the jail root. It returns
// the resolved real absolute path, or errEscape / errNotFound. Symlinks are resolved and the result
// re-checked, so a symlink pointing outside /workspace is rejected rather than followed.
func (s *Server) jail(raw string) (string, error) {
	rel, err := cleanRel(raw)
	if err != nil {
		return "", err
	}
	abs := filepath.Join(s.realRoot, rel)
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", errNotFound
		}
		return "", err
	}
	if !within(s.realRoot, real) {
		return "", errEscape
	}
	return real, nil
}

// cleanRel normalises a client path to a jail-relative, traversal-free path. Leading slashes are
// stripped so an "absolute" path is treated as relative to the jail; a cleaned path that still starts
// with ".." escapes the root and is rejected.
func cleanRel(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	p = strings.TrimLeft(p, "/")
	if p == "" {
		return ".", nil
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errEscape
	}
	return clean, nil
}

// within reports whether p is root itself or nested under root (canonical, separator-aware).
func within(root, p string) bool {
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	real, err := s.jail(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	fi, err := os.Stat(real)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !fi.IsDir() {
		http.Error(w, "not a directory", http.StatusBadRequest)
		return
	}
	des, err := os.ReadDir(real)
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	sort.Slice(des, func(i, j int) bool { return des[i].Name() < des[j].Name() })

	page := parseNonNegInt(r.URL.Query().Get("page"))
	start := page * s.pageSize
	if start > len(des) {
		start = len(des)
	}
	end := start + s.pageSize
	next := 0
	if end < len(des) {
		next = page + 1
	} else {
		end = len(des)
	}

	entries := make([]Entry, 0, end-start)
	for _, de := range des[start:end] {
		e := Entry{Name: de.Name(), Type: "file"}
		if de.IsDir() {
			e.Type = "dir"
		} else if info, ierr := de.Info(); ierr == nil {
			e.Size = info.Size()
		}
		entries = append(entries, e)
	}
	writeJSON(w, DirListing{Entries: entries, NextPage: next})
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if strings.TrimSpace(q.Get("path")) == "" {
		http.Error(w, "path query param required", http.StatusBadRequest)
		return
	}
	real, err := s.jail(q.Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	fi, err := os.Stat(real)
	if err != nil {
		writeErr(w, err)
		return
	}
	if fi.IsDir() {
		http.Error(w, "path is a directory", http.StatusBadRequest)
		return
	}

	size := fi.Size()
	offset := parseNonNegInt64(q.Get("offset"))
	if offset > size {
		offset = size
	}
	// Effective length: caller's length, else the cap; always clamped to the cap and to EOF so a
	// response is never an unbounded stream (AC4).
	eff := parseNonNegInt64(q.Get("length"))
	if eff == 0 || eff > s.maxBytes {
		eff = s.maxBytes
	}
	if offset+eff > size {
		eff = size - offset
	}

	data := make([]byte, eff)
	if eff > 0 {
		f, oerr := os.Open(real)
		if oerr != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		n, rerr := io.ReadFull(io.NewSectionReader(f, offset, eff), data)
		if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		data = data[:n]
	}

	ct := "text"
	if looksBinary(data) {
		ct = "binary"
	}
	writeJSON(w, FileContent{
		Size:        size,
		ContentType: ct,
		Offset:      offset,
		Length:      int64(len(data)),
		Data:        data,
	})
}

// looksBinary flags content the apiserver must not render as text: a NUL byte in the sniff window, or
// invalid UTF-8, is treated as binary (the git-native heuristic, mirroring buildbrowser scanNUL).
func looksBinary(b []byte) bool {
	sniff := b
	if len(sniff) > sniffLen {
		sniff = sniff[:sniffLen]
	}
	if bytes.IndexByte(sniff, 0) >= 0 {
		return true
	}
	return !utf8.Valid(sniff)
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, errEscape):
		http.Error(w, "path escapes workspace root", http.StatusBadRequest)
	default:
		http.Error(w, "read error", http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

func parseNonNegInt(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func parseNonNegInt64(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
