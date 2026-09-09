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

package readclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/K8squad/K8squad/internal/buildbrowser/readerpod/readserver"
)

// TestClient_List_Read_HappyPath: the client hits /list and /read with the expected query params and
// decodes the readserver wire types.
func TestClient_List_Read_HappyPath(t *testing.T) {
	var gotList, gotRead string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/list":
			gotList = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(readserver.DirListing{
				Entries:  []readserver.Entry{{Name: "main.go", Type: "file", Size: 42}, {Name: "pkg", Type: "dir"}},
				NextPage: 2,
			})
		case "/read":
			gotRead = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(readserver.FileContent{
				Size: 42, ContentType: "text", Offset: 0, Length: 5, Data: []byte("hello"),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, srv.Client())

	dl, err := c.List(context.Background(), "sub/dir", 2)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(dl.Entries) != 2 || dl.Entries[0].Name != "main.go" || dl.NextPage != 2 {
		t.Errorf("List decoded = %+v", dl)
	}
	if gotList != "page=2&path=sub%2Fdir" {
		t.Errorf("List query = %q", gotList)
	}

	fc, err := c.Read(context.Background(), "main.go", 0, 5)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(fc.Data) != "hello" || fc.ContentType != "text" || fc.Size != 42 {
		t.Errorf("Read decoded = %+v", fc)
	}
	if gotRead != "length=5&path=main.go" {
		t.Errorf("Read query = %q", gotRead)
	}
}

// TestClient_NonOKStatus_MapsToStatusError: a non-2xx reader response surfaces as a *StatusError
// carrying the code, so the apiserver can classify 404 vs 400.
func TestClient_NonOKStatus_MapsToStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL, srv.Client())
	_, err := c.List(context.Background(), "missing", 0)
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *StatusError", err)
	}
	if se.Code != http.StatusNotFound {
		t.Errorf("StatusError.Code = %d, want 404", se.Code)
	}
}

// TestClient_EmptyBaseURL_Fails: a client with no base URL fails closed rather than dialing nothing.
func TestClient_EmptyBaseURL_Fails(t *testing.T) {
	c := New("", nil)
	if _, err := c.List(context.Background(), ".", 0); err == nil {
		t.Fatal("List with empty base URL should error")
	}
}
