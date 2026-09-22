package apiserver

// pathvars.go — the single seam that reverses the router's UseEncodedPath()
// escaping for path variables (ISI-4795).
//
// NewServer runs the root router with UseEncodedPath()+SkipClean(true) so a
// Project's canonical "namespace/name" composite id (ISI-3982) survives routing
// as a single {projectId} segment ("namespace%2Fname") instead of being split
// into two path segments — the reported project-runs/overview 404. The cost of
// UseEncodedPath() is that EVERY captured var is now the ESCAPED form, so each
// reader must unescape once to recover the value handlers saw under the default
// decoded-path routing. decodePathVar is that reversal; it is applied uniformly
// by the shared var helpers (pathVar, muxVar, muxVarsName) and at each inline
// mux.Vars read, so no handler has to think about the router mode.

import "net/url"

// decodePathVar reverses the single layer of percent-encoding a mux path
// variable carries under UseEncodedPath(). PathUnescape of a value with no
// escapes is the identity, so this is safe for UUIDs, issue numbers, and
// DNS-label names alike — only the "namespace%2Fname" Project composite (and any
// other var that legitimately carried an escape under decoded-path routing) is
// changed. A malformed escape falls back to the raw value rather than dropping
// the var, so a bad request degrades to a not-found, never a panic.
func decodePathVar(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}
