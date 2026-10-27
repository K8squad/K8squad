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

package apiserver

// ISI-6749: restore the internal/apiserver coverage floor (74.8 → 75.7) that the
// ISI-6539 baseline refresh re-pinned downward. These tests exercise the
// error/branch paths the existing suite left uncovered — the host config loader
// and validator, the JWT-key decoder, the Project ref/UID resolvers, the
// decision_request error mapper, and the numeric query-param parsers — without
// touching production code.

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// --- config.go: LoadConfig / Validate / decodeKeyBytes (DecodeJWTKey) ---

func TestLoadConfig(t *testing.T) {
	t.Run("empty path returns defaults", func(t *testing.T) {
		cfg, err := LoadConfig("")
		if err != nil {
			t.Fatalf("LoadConfig(\"\") err = %v", err)
		}
		if cfg.HTTPPort != DefaultConfig().HTTPPort {
			t.Errorf("port = %d, want default %d", cfg.HTTPPort, DefaultConfig().HTTPPort)
		}
	})

	t.Run("missing file returns defaults (run from flags/env)", func(t *testing.T) {
		cfg, err := LoadConfig(filepath.Join(t.TempDir(), "nope.json"))
		if err != nil {
			t.Fatalf("missing file err = %v, want nil", err)
		}
		if cfg.SessionCookie != SessionCookieName {
			t.Errorf("sessionCookie = %q, want default", cfg.SessionCookie)
		}
	})

	t.Run("valid file overrides defaults", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "cfg.json")
		if err := os.WriteFile(p, []byte(`{"httpPort":9090,"sessionCookie":"ksq_sess"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(p)
		if err != nil {
			t.Fatalf("LoadConfig err = %v", err)
		}
		if cfg.HTTPPort != 9090 || cfg.SessionCookie != "ksq_sess" {
			t.Errorf("got port=%d cookie=%q", cfg.HTTPPort, cfg.SessionCookie)
		}
	})

	t.Run("empty sessionCookie refills the default", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "cfg.json")
		if err := os.WriteFile(p, []byte(`{"sessionCookie":""}`), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(p)
		if err != nil {
			t.Fatalf("LoadConfig err = %v", err)
		}
		if cfg.SessionCookie != SessionCookieName {
			t.Errorf("sessionCookie = %q, want refilled default", cfg.SessionCookie)
		}
	})

	t.Run("malformed JSON is a parse error", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(p, []byte(`{not json`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(p); err == nil {
			t.Fatal("malformed JSON must return a parse error")
		}
	})

	t.Run("unreadable path (a directory) is a read error", func(t *testing.T) {
		if _, err := LoadConfig(t.TempDir()); err == nil {
			t.Fatal("reading a directory as a config file must error")
		}
	})
}

func TestConfigValidate(t *testing.T) {
	base := DefaultConfig()

	if err := base.Validate(); err != nil {
		t.Fatalf("default config must validate, got %v", err)
	}

	t.Run("empty databaseUrl fails closed", func(t *testing.T) {
		c := base
		c.DatabaseURL = ""
		if err := c.Validate(); err == nil {
			t.Fatal("empty databaseUrl must fail validation")
		}
	})

	t.Run("out-of-range port fails closed", func(t *testing.T) {
		for _, port := range []int{0, -1, 70000} {
			c := base
			c.HTTPPort = port
			if err := c.Validate(); err == nil {
				t.Errorf("port %d must fail validation", port)
			}
		}
	})

	t.Run("short jwt signing key fails closed", func(t *testing.T) {
		c := base
		c.JWTSigningKey = "too-short"
		if err := c.Validate(); err == nil {
			t.Fatal("a sub-32-byte jwt key must fail validation")
		}
	})

	t.Run("a 32-byte raw jwt key validates", func(t *testing.T) {
		c := base
		c.JWTSigningKey = string(make([]byte, 32))
		if err := c.Validate(); err != nil {
			t.Fatalf("a 32-byte raw key must validate, got %v", err)
		}
	})
}

func TestDecodeJWTKey(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}

	t.Run("unpadded standard base64 (GenerateSigningKey form) decodes", func(t *testing.T) {
		enc := base64.RawStdEncoding.EncodeToString(raw)
		if got := DecodeJWTKey(enc); string(got) != string(raw) {
			t.Errorf("raw-std decode mismatch: len(got)=%d", len(got))
		}
	})

	t.Run("padded standard base64 decodes", func(t *testing.T) {
		enc := base64.StdEncoding.EncodeToString(raw)
		if got := DecodeJWTKey(enc); string(got) != string(raw) {
			t.Errorf("padded-std decode mismatch: len(got)=%d", len(got))
		}
	})

	t.Run("a raw (non-base64) key passes through as its own bytes", func(t *testing.T) {
		// 32+ printable ASCII bytes that are not valid base64 of >=32 bytes.
		key := "this-is-a-plain-text-signing-key-over-32-bytes!!"
		if got := DecodeJWTKey(key); string(got) != key {
			t.Errorf("raw passthrough mismatch: got %q", string(got))
		}
	})
}

// --- projectresolve.go: NewClientProjectRefResolver / ResolveProjectRef ---

func TestResolveProjectRef(t *testing.T) {
	const ns = "bmad-squad"
	proj := &ksquadv1.Project{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bmad-demo", UID: types.UID("uid-123")},
	}
	tm := team(ns, "bmad-team", "7191cc8c-f4b7-4b60-b63e-d25408ac0d1c")

	newResolver := func(objs ...client.Object) ProjectRefResolver {
		c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(objs...).Build()
		return NewClientProjectRefResolver(c)
	}

	t.Run("empty ref is not found (existence hiding)", func(t *testing.T) {
		if _, err := newResolver().ResolveProjectRef(context.Background(), ""); !errors.Is(err, ErrProjectNotFound) {
			t.Fatalf("err = %v, want ErrProjectNotFound", err)
		}
	})

	t.Run("UID match wins and carries the owning team", func(t *testing.T) {
		res, err := newResolver(proj, tm).ResolveProjectRef(context.Background(), "uid-123")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if res.UID != "uid-123" || res.Namespace != ns || res.Name != "bmad-demo" {
			t.Errorf("resolution = %+v", res)
		}
		if res.TeamUID != string(tm.UID) {
			t.Errorf("teamUID = %q, want %q", res.TeamUID, tm.UID)
		}
	})

	t.Run("composite namespace/name match", func(t *testing.T) {
		res, err := newResolver(proj).ResolveProjectRef(context.Background(), ns+"/bmad-demo")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if res.UID != "uid-123" {
			t.Errorf("resolution = %+v", res)
		}
		// no Team CR present ⇒ TeamUID stays empty (teamInNamespace returns nil,nil)
		if res.TeamUID != "" {
			t.Errorf("teamUID = %q, want empty", res.TeamUID)
		}
	})

	t.Run("bare name match", func(t *testing.T) {
		res, err := newResolver(proj).ResolveProjectRef(context.Background(), "bmad-demo")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if res.Name != "bmad-demo" {
			t.Errorf("resolution = %+v", res)
		}
	})

	t.Run("bare-name collision across squads is ambiguous", func(t *testing.T) {
		a := &ksquadv1.Project{ObjectMeta: metav1.ObjectMeta{Namespace: "squad-a", Name: "dupe", UID: types.UID("a")}}
		b := &ksquadv1.Project{ObjectMeta: metav1.ObjectMeta{Namespace: "squad-b", Name: "dupe", UID: types.UID("b")}}
		if _, err := newResolver(a, b).ResolveProjectRef(context.Background(), "dupe"); !errors.Is(err, ErrProjectAmbiguous) {
			t.Fatalf("err = %v, want ErrProjectAmbiguous", err)
		}
	})

	t.Run("no match is not found", func(t *testing.T) {
		if _, err := newResolver(proj).ResolveProjectRef(context.Background(), "ghost"); !errors.Is(err, ErrProjectNotFound) {
			t.Fatalf("err = %v, want ErrProjectNotFound", err)
		}
	})

	t.Run("a List failure surfaces, not a false not-found", func(t *testing.T) {
		boom := errors.New("informer cache read failed")
		c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return boom
			},
		}).Build()
		if _, err := NewClientProjectRefResolver(c).ResolveProjectRef(context.Background(), "uid-123"); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the underlying List error", err)
		}
	})
}

// --- projectuidresolve.go: NewProjectUIDResolver / ResolveProjectUID ---

func TestProjectUIDResolver(t *testing.T) {
	if NewProjectUIDResolver(nil) != nil {
		t.Fatal("nil refs must yield a nil resolver")
	}

	t.Run("resolved UID passes through ok=true", func(t *testing.T) {
		r := NewProjectUIDResolver(&fakeProjectRefs{res: ProjectRefResolution{UID: "uid-9"}})
		uid, ok, err := r.ResolveProjectUID(context.Background(), "p")
		if err != nil || !ok || uid != "uid-9" {
			t.Fatalf("got uid=%q ok=%v err=%v", uid, ok, err)
		}
	})

	t.Run("empty UID resolution ⇒ ok=false", func(t *testing.T) {
		r := NewProjectUIDResolver(&fakeProjectRefs{res: ProjectRefResolution{}})
		if _, ok, err := r.ResolveProjectUID(context.Background(), "p"); ok || err != nil {
			t.Fatalf("got ok=%v err=%v, want ok=false err=nil", ok, err)
		}
	})

	t.Run("not-found / ambiguous ⇒ ok=false, no error", func(t *testing.T) {
		for _, sentinel := range []error{ErrProjectNotFound, ErrProjectAmbiguous} {
			r := NewProjectUIDResolver(&fakeProjectRefs{err: sentinel})
			if _, ok, err := r.ResolveProjectUID(context.Background(), "p"); ok || err != nil {
				t.Errorf("sentinel %v: got ok=%v err=%v, want ok=false err=nil", sentinel, ok, err)
			}
		}
	})

	t.Run("a genuine infra error surfaces", func(t *testing.T) {
		boom := errors.New("cache down")
		r := NewProjectUIDResolver(&fakeProjectRefs{err: boom})
		if _, ok, err := r.ResolveProjectUID(context.Background(), "p"); ok || !errors.Is(err, boom) {
			t.Fatalf("got ok=%v err=%v, want ok=false err=boom", ok, err)
		}
	})
}

// --- decisionrequest.go: mapDecisionErr ---

func TestMapDecisionErr(t *testing.T) {
	t.Run("nil error maps nothing and returns false", func(t *testing.T) {
		rec := httptest.NewRecorder()
		if mapDecisionErr(rec, nil) {
			t.Fatal("nil error must return false (not handled)")
		}
		if rec.Code != http.StatusOK {
			t.Errorf("nil error wrote status %d", rec.Code)
		}
	})

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not found → 404", discussion.ErrDecisionRequestNotFound, http.StatusNotFound},
		{"not open → 409", discussion.ErrDecisionRequestNotOpen, http.StatusConflict},
		{"reason required → 400", discussion.ErrRejectReasonRequired, http.StatusBadRequest},
		{"invalid payload → 400", discussion.ErrInvalidDecisionPayload, http.StatusBadRequest},
		{"unknown → 500", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if !mapDecisionErr(rec, tc.err) {
				t.Fatal("a non-nil error must return true (handled)")
			}
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// --- files.go: parseIntParam / parseInt64Param ---

func TestParseIntParams(t *testing.T) {
	t.Run("parseIntParam valid writes through", func(t *testing.T) {
		var dst int
		v, err := parseIntParam("42", &dst)
		if err != nil || v != 42 || dst != 42 {
			t.Fatalf("got v=%d dst=%d err=%v", v, dst, err)
		}
	})

	t.Run("parseIntParam invalid errors and leaves dst untouched", func(t *testing.T) {
		dst := 7
		if _, err := parseIntParam("not-a-number", &dst); err == nil {
			t.Fatal("non-numeric input must error")
		}
		if dst != 7 {
			t.Errorf("dst mutated to %d on error", dst)
		}
	})

	t.Run("parseInt64Param valid writes through", func(t *testing.T) {
		var dst int64
		v, err := parseInt64Param("9000000000", &dst)
		if err != nil || v != 9000000000 || dst != 9000000000 {
			t.Fatalf("got v=%d dst=%d err=%v", v, dst, err)
		}
	})

	t.Run("parseInt64Param invalid errors", func(t *testing.T) {
		var dst int64
		if _, err := parseInt64Param("x", &dst); err == nil {
			t.Fatal("non-numeric input must error")
		}
	})
}

// --- audit.go: parseInt ---

func TestAuditParseInt(t *testing.T) {
	if v, err := parseInt("123"); err != nil || v != 123 {
		t.Fatalf("parseInt(\"123\") = %d, %v", v, err)
	}
	if _, err := parseInt("abc"); err == nil {
		t.Fatal("parseInt(\"abc\") must error")
	}
}
