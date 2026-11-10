package main

import (
	"context"
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/K8squad/K8squad/internal/apiserver"
	"github.com/K8squad/K8squad/pkg/auth"
)

// ISI-7286 (L5 green-up): buildAuthService, the dbReady ping adapter, and the
// coord audit sink were the zero-coverage remainder of cmd/apiserver. Pin the
// startup wiring contracts here so the ratchet floor stays earned, not lowered.

// TestBuildAuthServiceAutoKey pins the ADR-033 dev posture: with no signing key
// configured the service still assembles by auto-generating a 32-byte key, and
// an unqueryable auth schema stays non-fatal (fail visible, warning logged).
func TestBuildAuthServiceAutoKey(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	// No query expectations: users.Count fails, exercising the fail-open warning.

	cfg := apiserver.Config{
		JWTTTLSeconds:          3600,
		SessionTTLSeconds:      3600,
		MaxHashConcurrency:     0, // defaulting arm
		LoginRateLimit:         5,
		LoginRateWindowSeconds: 60,
	}
	if svc := buildAuthService(context.Background(), db, cfg); svc == nil {
		t.Fatal("buildAuthService returned nil with auto-generated key")
	}
}

// TestBuildAuthServiceValidGroupMapping covers the 15.9 startup-validation seam:
// a parseable oidcGroupMapping is accepted (and logged), a configured key is
// used as-is instead of the auto-generation path.
func TestBuildAuthServiceValidGroupMapping(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	cfg := apiserver.Config{
		JWTSigningKey:          auth.GenerateSigningKey(),
		JWTTTLSeconds:          3600,
		SessionTTLSeconds:      3600,
		MaxHashConcurrency:     2,
		LoginRateLimit:         5,
		LoginRateWindowSeconds: 60,
		OidcGroupMapping:       "{}",
	}
	if svc := buildAuthService(context.Background(), db, cfg); svc == nil {
		t.Fatal("buildAuthService returned nil with configured key + groupMapping")
	}
}

// TestDBReady pins the /readyz contract: green iff the ping succeeds.
func TestDBReady(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectPing()
	if err := (dbReady{db}).Ready(context.Background()); err != nil {
		t.Fatalf("Ready: want nil on ping success, got %v", err)
	}

	mock.ExpectPing().WillReturnError(errors.New("db down"))
	if err := (dbReady{db}).Ready(context.Background()); err == nil {
		t.Fatal("Ready: want error on ping failure, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestCoordAuditWriter pins ADR-040 behavior: successful appends run the
// coord.audit_log INSERT; a failed append is logged, never panics; an
// unmarshalable payload degrades to a log line without touching the DB.
func TestCoordAuditWriter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec("INSERT INTO coord.audit_log").
		WithArgs("user.created", "local-board", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	w := coordAuditWriter(db)
	w(context.Background(), "user.created", "local-board", map[string]any{"ok": true})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}

	// Failed append: loud log, no panic.
	mock.ExpectExec("INSERT INTO coord.audit_log").
		WithArgs("user.deleted", "local-board", sqlmock.AnyArg()).
		WillReturnError(errors.New("coord db gone"))
	w(context.Background(), "user.deleted", "local-board", map[string]any{"ok": true})

	// Unmarshalable payload (chan): marshal-error arm, no DB call.
	w(context.Background(), "user.noop", "local-board", map[string]any{"ch": make(chan int)})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}
