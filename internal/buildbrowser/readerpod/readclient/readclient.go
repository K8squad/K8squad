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

// Package readclient is the apiserver-side HTTP client for the story 8.7f / S4a reader-pod protocol
// (ISI-4005 / S4b-wire ISI-4072). It reaches the reader pod ONLY over its in-cluster ClusterIP
// (Handle.BaseURL) — GET /list and GET /read — and decodes the readserver wire types. There is NO
// pods/exec, NO kubectl cp, NO PVC mount here: this is a plain net/http client, so it carries ZERO
// Kubernetes-client dependencies and is unit-testable against an httptest server.
//
// Containment: the client is a thin transport. The pod already jails, paginates, byte-caps and
// byte-ranges every response (readserver AC3/AC4); the apiserver defence-in-depth jail runs before
// this client is called (files.go workspaceJailPath). The client adds only a response byte-cap on
// the read path so a compromised/misbehaving pod cannot stream unbounded bytes into the apiserver.
package readclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/K8squad/K8squad/internal/buildbrowser/readerpod/readserver"
)

// maxResponseBytes caps how much of a reader-pod response the client will read into memory. The pod
// already caps a single /read at 1 MiB (readserver.maxReadBytes) and a listing page at 1000 entries;
// this is the client-side backstop (a few MiB of slack for JSON+base64 framing) so a misbehaving pod
// cannot OOM the apiserver.
const maxResponseBytes = 8 << 20 // 8 MiB

// defaultTimeout bounds a single reader-pod call so a hung pod degrades to an error the S4b route can
// map, rather than tying up an apiserver request goroutine indefinitely.
const defaultTimeout = 15 * time.Second

// Client is a reader-pod protocol client bound to one reader's in-cluster base URL.
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds a Client for a reader-pod base URL (Handle.BaseURL, e.g.
// http://buildreader-<run>.<ns>.svc:8080). An empty baseURL is a programming error surfaced on the
// first call. A nil hc uses a default client with a bounded per-call timeout.
func New(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{baseURL: baseURL, http: hc}
}

// List calls GET /list?path=&page= and returns the pod's DirListing wire value. path is the
// jail-relative directory (already canonicalised by the caller); page is a zero-based page index.
func (c *Client) List(ctx context.Context, path string, page int) (readserver.DirListing, error) {
	q := url.Values{}
	q.Set("path", path)
	if page > 0 {
		q.Set("page", strconv.Itoa(page))
	}
	var out readserver.DirListing
	if err := c.getJSON(ctx, "/list", q, &out); err != nil {
		return readserver.DirListing{}, err
	}
	return out, nil
}

// Read calls GET /read?path=&offset=&length= and returns the pod's FileContent wire value. A zero
// length means "from offset to the pod's byte cap"; the pod always clamps the slice to its own cap.
func (c *Client) Read(ctx context.Context, path string, offset, length int64) (readserver.FileContent, error) {
	q := url.Values{}
	q.Set("path", path)
	if offset > 0 {
		q.Set("offset", strconv.FormatInt(offset, 10))
	}
	if length > 0 {
		q.Set("length", strconv.FormatInt(length, 10))
	}
	var out readserver.FileContent
	if err := c.getJSON(ctx, "/read", q, &out); err != nil {
		return readserver.FileContent{}, err
	}
	return out, nil
}

// getJSON performs a bounded GET against the reader pod and decodes a JSON body into out. A non-2xx
// status is turned into an error carrying the status code so the caller can map 404→not-found etc.
func (c *Client) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	if c.baseURL == "" {
		return fmt.Errorf("readclient: empty base URL (reader not launched?)")
	}
	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("readclient: build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("readclient: %s: %w", path, err)
	}
	defer resp.Body.Close()

	body := io.LimitReader(resp.Body, maxResponseBytes)
	if resp.StatusCode != http.StatusOK {
		// Drain a little of the body for the error message, then classify by status.
		snippet, _ := io.ReadAll(io.LimitReader(body, 512))
		return &StatusError{Code: resp.StatusCode, Body: string(snippet)}
	}
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return fmt.Errorf("readclient: decode %s response: %w", path, err)
	}
	return nil
}

// StatusError is a non-2xx reader-pod response. The apiserver WorkspaceReader maps 404→not-found and
// 400→bad-request so the S4b route can answer honestly without leaking pod internals.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("readclient: reader pod returned HTTP %d: %s", e.Code, e.Body)
}
