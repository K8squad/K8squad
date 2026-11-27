package idemkey

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGetPutRoundTrip(t *testing.T) {
	c := NewReplayCache(time.Minute)
	if _, _, ok := c.Get("scope"); ok {
		t.Fatal("empty cache must miss")
	}
	c.Put("scope", http.StatusCreated, []byte("ok:true"))
	status, body, ok := c.Get("scope")
	if !ok || status != http.StatusCreated || string(body) != "ok:true" {
		t.Fatalf("round trip got (%d, %q, %v)", status, body, ok)
	}
}

func TestExpiry(t *testing.T) {
	c := NewReplayCache(20 * time.Millisecond)
	c.Put("scope", http.StatusCreated, []byte("x"))
	if _, _, ok := c.Get("scope"); !ok {
		t.Fatal("fresh entry must hit")
	}
	time.Sleep(30 * time.Millisecond)
	if _, _, ok := c.Get("scope"); ok {
		t.Fatal("expired entry must miss")
	}
	// The expired slot must not block a re-Put (window reset).
	c.Put("scope", http.StatusCreated, []byte("y"))
	if _, body, ok := c.Get("scope"); !ok || string(body) != "y" {
		t.Fatalf("re-Put after expiry got (%q, %v)", body, ok)
	}
}

func TestPutFirstWriteWins(t *testing.T) {
	c := NewReplayCache(time.Minute)
	c.Put("scope", http.StatusCreated, []byte("first"))
	c.Put("scope", http.StatusCreated, []byte("second"))
	_, body, ok := c.Get("scope")
	if !ok || string(body) != "first" {
		t.Fatalf("first write must win, got (%q, %v)", body, ok)
	}
}

func TestKeyOfScoping(t *testing.T) {
	base := KeyOf("principal-a", "target-1", "key-1")
	if base != KeyOf("principal-a", "target-1", "key-1") {
		t.Fatal("identical inputs must scope equal")
	}
	for _, other := range []string{
		KeyOf("principal-b", "target-1", "key-1"),
		KeyOf("principal-a", "target-2", "key-1"),
		KeyOf("principal-a", "target-1", "key-2"),
	} {
		if base == other {
			t.Fatal("different inputs must not collide")
		}
	}
}

func TestServeJSONReplayHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	ServeJSON(rec, http.StatusCreated, []byte("{}"), true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get(ReplayHeader); got != "true" {
		t.Fatalf("replay header = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q", ct)
	}

	fresh := httptest.NewRecorder()
	ServeJSON(fresh, http.StatusCreated, []byte("{}"), false)
	if fresh.Header().Get(ReplayHeader) != "" {
		t.Fatal("fresh response must not carry the replay header")
	}
}

func TestDefaultWindowFallback(t *testing.T) {
	if NewReplayCache(0).window != DefaultWindow {
		t.Fatal("non-positive window must fall back to DefaultWindow")
	}
}
