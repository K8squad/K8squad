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

package auth

// ISI-6747 coverage restore: the Postgres-backed stores (user, session, membership)
// were re-pinned DOWN in the ISI-6539 baseline refresh because their query/branch
// paths had no unit coverage. These sqlmock-driven tests exercise those paths
// (no production-code changes) to climb pkg/auth back above its old floor.
// Pattern reference: pkg/controller/contextsource/source_sql_test.go (ISI, #958).

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func newUserStore(t *testing.T) (*PostgresUserStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewPostgresUserStore(db), mock
}

// userRows builds a single-row result in the exact column order scanUser expects.
func userRows(u *User) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "username", "principal", "email", "password_hash",
		"team_id", "global_role", "created_at", "created_by", "deactivated_at",
	}).AddRow(u.ID.String(), u.Username, u.Principal, u.Email, u.PasswordHash,
		u.TeamID.String(), u.GlobalRole, u.CreatedAt, u.CreatedBy, u.DeactivatedAt)
}

func sampleUser() *User {
	return &User{
		ID:         uuid.New(),
		Username:   "alice",
		Principal:  "user:alice",
		TeamID:     uuid.New(),
		GlobalRole: RoleAdmin,
		CreatedAt:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func TestPostgresUserStore_ByUsername_And_ByID(t *testing.T) {
	s, mock := newUserStore(t)
	ctx := context.Background()
	want := sampleUser()

	mock.ExpectQuery(`FROM auth.user WHERE username`).WithArgs("alice").
		WillReturnRows(userRows(want))
	got, err := s.ByUsername(ctx, "alice")
	if err != nil || got.ID != want.ID {
		t.Fatalf("ByUsername = %+v, %v", got, err)
	}

	mock.ExpectQuery(`FROM auth.user WHERE id`).WithArgs(want.ID).
		WillReturnRows(userRows(want))
	got, err = s.ByID(ctx, want.ID)
	if err != nil || got.Username != "alice" {
		t.Fatalf("ByID = %+v, %v", got, err)
	}

	// ErrNotFound path through scanUser.
	mock.ExpectQuery(`FROM auth.user WHERE username`).WithArgs("ghost").
		WillReturnError(sql.ErrNoRows)
	if _, err := s.ByUsername(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	// Wrapped non-sentinel error path through scanUser.
	mock.ExpectQuery(`FROM auth.user WHERE id`).WithArgs(want.ID).
		WillReturnError(errors.New("boom"))
	if _, err := s.ByID(ctx, want.ID); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want wrapped error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresUserStore_Create_DefaultsPrincipalAndRole(t *testing.T) {
	s, mock := newUserStore(t)
	ctx := context.Background()
	id := uuid.New()
	created := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)

	// Principal + role empty → defaulted before the INSERT.
	u := &User{Username: "bob", TeamID: uuid.New()}
	mock.ExpectQuery(`INSERT INTO auth.user`).
		WithArgs("bob", "user:bob", "", u.TeamID, RoleUser, u.Email, u.CreatedBy).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(id.String(), created))
	if err := s.Create(ctx, u); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if u.Principal != "user:bob" || u.GlobalRole != RoleUser || u.ID != id {
		t.Fatalf("defaults not applied: %+v", u)
	}

	// Error path.
	u2 := &User{Username: "err", Principal: "user:err", GlobalRole: RoleUser, TeamID: uuid.New()}
	mock.ExpectQuery(`INSERT INTO auth.user`).WillReturnError(errors.New("dup"))
	if err := s.Create(ctx, u2); err == nil {
		t.Fatal("want create error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresUserStore_List(t *testing.T) {
	s, mock := newUserStore(t)
	ctx := context.Background()
	u := sampleUser()

	// limit<=0 is clamped to 50; offset<0 clamped to 0.
	mock.ExpectQuery(`FROM auth.user ORDER BY created_at`).WithArgs(50, 0).
		WillReturnRows(userRows(u))
	mock.ExpectQuery(`SELECT count\(\*\) FROM auth.user`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	list, total, err := s.List(ctx, 0, -5)
	if err != nil || total != 1 || len(list) != 1 {
		t.Fatalf("List = %v total=%d err=%v", list, total, err)
	}

	// Query error path.
	mock.ExpectQuery(`FROM auth.user ORDER BY created_at`).WillReturnError(errors.New("boom"))
	if _, _, err := s.List(ctx, 10, 0); err == nil {
		t.Fatal("want list error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresUserStore_Update(t *testing.T) {
	ctx := context.Background()
	admin := RoleAdmin
	user := RoleUser
	bad := "superuser"
	id := uuid.New()

	t.Run("invalid role rejected before any query", func(t *testing.T) {
		s, _ := newUserStore(t)
		if _, err := s.Update(ctx, id, UserUpdate{GlobalRole: &bad}); err == nil {
			t.Fatal("want validation error")
		}
	})

	t.Run("last admin demotion refused", func(t *testing.T) {
		s, mock := newUserStore(t)
		mock.ExpectBegin()
		mock.ExpectQuery(`count\(\*\) FROM auth.user`).WithArgs(id).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
		mock.ExpectRollback()
		if _, err := s.Update(ctx, id, UserUpdate{GlobalRole: &user}); !errors.Is(err, ErrLastAdmin) {
			t.Fatalf("want ErrLastAdmin, got %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("role+email update commits and reloads", func(t *testing.T) {
		s, mock := newUserStore(t)
		email := "a@b.c"
		reloaded := sampleUser()
		reloaded.ID = id
		mock.ExpectBegin()
		// promotion to admin skips the last-admin guard
		mock.ExpectExec(`UPDATE auth.user SET global_role`).WithArgs(admin, id).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(`UPDATE auth.user SET email`).WithArgs(email, id).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
		mock.ExpectQuery(`FROM auth.user WHERE id`).WithArgs(id).
			WillReturnRows(userRows(reloaded))
		got, err := s.Update(ctx, id, UserUpdate{GlobalRole: &admin, Email: &email})
		if err != nil || got.ID != id {
			t.Fatalf("Update = %+v, %v", got, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPostgresUserStore_UpdatePassword(t *testing.T) {
	s, mock := newUserStore(t)
	ctx := context.Background()
	id := uuid.New()

	mock.ExpectExec(`UPDATE auth.user SET password_hash`).WithArgs("$argon2id$hash", id).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := s.UpdatePassword(ctx, id, "$argon2id$hash"); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}

	// 0 rows affected → ErrNotFound.
	mock.ExpectExec(`UPDATE auth.user SET password_hash`).WithArgs("$x", id).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := s.UpdatePassword(ctx, id, "$x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresUserStore_Deactivate(t *testing.T) {
	ctx := context.Background()
	id := uuid.New()

	t.Run("non-admin deactivated", func(t *testing.T) {
		s, mock := newUserStore(t)
		mock.ExpectBegin()
		mock.ExpectQuery(`global_role = 'admin'`).WithArgs(id).
			WillReturnRows(sqlmock.NewRows([]string{"is_admin"}).AddRow(false))
		mock.ExpectExec(`SET deactivated_at = now`).WithArgs(id).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
		if err := s.Deactivate(ctx, id); err != nil {
			t.Fatalf("Deactivate: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unknown user", func(t *testing.T) {
		s, mock := newUserStore(t)
		mock.ExpectBegin()
		mock.ExpectQuery(`global_role = 'admin'`).WithArgs(id).WillReturnError(sql.ErrNoRows)
		mock.ExpectRollback()
		if err := s.Deactivate(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("last admin refused", func(t *testing.T) {
		s, mock := newUserStore(t)
		mock.ExpectBegin()
		mock.ExpectQuery(`global_role = 'admin'`).WithArgs(id).
			WillReturnRows(sqlmock.NewRows([]string{"is_admin"}).AddRow(true))
		mock.ExpectQuery(`count\(\*\) FROM auth.user`).WithArgs(id).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
		mock.ExpectRollback()
		if err := s.Deactivate(ctx, id); !errors.Is(err, ErrLastAdmin) {
			t.Fatalf("want ErrLastAdmin, got %v", err)
		}
	})
}

func TestPostgresUserStore_Count(t *testing.T) {
	s, mock := newUserStore(t)
	ctx := context.Background()
	mock.ExpectQuery(`SELECT count\(\*\) FROM auth.user`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))
	n, err := s.Count(ctx)
	if err != nil || n != 7 {
		t.Fatalf("Count = %d, %v", n, err)
	}
}

func TestPostgresUserStore_UpdateTeamID(t *testing.T) {
	s, mock := newUserStore(t)
	ctx := context.Background()
	from, to, resultID := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectQuery(`UPDATE auth.user SET team_id`).WithArgs(to, "user:alice", from).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(resultID.String()))
	got, err := s.UpdateTeamID(ctx, "user:alice", from, to)
	if err != nil || got != resultID {
		t.Fatalf("UpdateTeamID = %v, %v", got, err)
	}

	// CAS matched nothing → ErrNotFound (the "nothing to rebind" signal).
	mock.ExpectQuery(`UPDATE auth.user SET team_id`).WithArgs(to, "user:ghost", from).
		WillReturnError(sql.ErrNoRows)
	if _, err := s.UpdateTeamID(ctx, "user:ghost", from, to); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---- Session store ---------------------------------------------------------

func newSessionStore(t *testing.T) (*PostgresSessionStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewPostgresSessionStore(db), mock
}

func TestPostgresSessionStore_Create(t *testing.T) {
	s, mock := newSessionStore(t)
	ctx := context.Background()
	uid, sid := uuid.New(), uuid.New()
	exp := time.Now().Add(time.Hour)

	mock.ExpectQuery(`INSERT INTO auth.session`).
		WithArgs(sqlmock.AnyArg(), uid, "3600").
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "expires_at"}).
			AddRow(sid.String(), uid.String(), exp))
	sess, err := s.Create(ctx, uid, time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.Token == "" || sess.ID != sid || sess.UserID != uid {
		t.Fatalf("session = %+v", sess)
	}
}

func TestPostgresSessionStore_Rotate(t *testing.T) {
	ctx := context.Background()

	t.Run("success", func(t *testing.T) {
		s, mock := newSessionStore(t)
		uid, newID := uuid.New(), uuid.New()
		exp := time.Now().Add(time.Hour)
		mock.ExpectBegin()
		mock.ExpectQuery(`UPDATE auth.session`).WithArgs(sqlmock.AnyArg()).
			WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(uid.String()))
		mock.ExpectQuery(`INSERT INTO auth.session`).
			WithArgs(sqlmock.AnyArg(), uid, "60").
			WillReturnRows(sqlmock.NewRows([]string{"id", "expires_at"}).AddRow(newID.String(), exp))
		mock.ExpectCommit()
		sess, err := s.Rotate(ctx, "old-token", time.Minute)
		if err != nil || sess.ID != newID || sess.UserID != uid || sess.Token == "" {
			t.Fatalf("Rotate = %+v, %v", sess, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("dead token fails closed", func(t *testing.T) {
		s, mock := newSessionStore(t)
		mock.ExpectBegin()
		mock.ExpectQuery(`UPDATE auth.session`).WillReturnError(sql.ErrNoRows)
		mock.ExpectRollback()
		if _, err := s.Rotate(ctx, "dead", time.Minute); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})
}

func TestPostgresSessionStore_RevokeAndPrune(t *testing.T) {
	s, mock := newSessionStore(t)
	ctx := context.Background()
	uid := uuid.New()

	mock.ExpectExec(`UPDATE auth.session SET revoked_at`).WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := s.Revoke(ctx, "tok"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	mock.ExpectExec(`SET revoked_at = now`).WithArgs(uid).
		WillReturnResult(sqlmock.NewResult(0, 3))
	if err := s.RevokeAllForUser(ctx, uid); err != nil {
		t.Fatalf("RevokeAllForUser: %v", err)
	}

	mock.ExpectExec(`DELETE FROM auth.session`).
		WillReturnResult(sqlmock.NewResult(0, 9))
	n, err := s.PruneExpired(ctx)
	if err != nil || n != 9 {
		t.Fatalf("PruneExpired = %d, %v", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresSessionStore_Resolve(t *testing.T) {
	s, mock := newSessionStore(t)
	ctx := context.Background()
	uid := uuid.New()

	// empty token short-circuits before any query.
	if _, err := s.Resolve(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty token: want ErrNotFound, got %v", err)
	}

	mock.ExpectQuery(`FROM auth.session`).WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(uid.String()))
	got, err := s.Resolve(ctx, "live")
	if err != nil || got != uid {
		t.Fatalf("Resolve = %v, %v", got, err)
	}

	mock.ExpectQuery(`FROM auth.session`).WithArgs(sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)
	if _, err := s.Resolve(ctx, "dead"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dead token: want ErrNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---- Membership store ------------------------------------------------------

func newMembershipStore(t *testing.T) (*PostgresMembershipStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewPostgresMembershipStore(db), mock
}

func TestPostgresMembershipStore_RoleForPrincipal(t *testing.T) {
	s, mock := newMembershipStore(t)
	ctx := context.Background()

	mock.ExpectQuery(`FROM auth.project_membership`).WithArgs("user:alice", "proj-1").
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow(ProjectRoleMaintainer))
	role, err := s.RoleForPrincipal(ctx, "user:alice", "proj-1")
	if err != nil || role != ProjectRoleMaintainer {
		t.Fatalf("RoleForPrincipal = %q, %v", role, err)
	}

	mock.ExpectQuery(`FROM auth.project_membership`).WithArgs("user:ghost", "proj-1").
		WillReturnError(sql.ErrNoRows)
	if _, err := s.RoleForPrincipal(ctx, "user:ghost", "proj-1"); !errors.Is(err, ErrNoMembership) {
		t.Fatalf("want ErrNoMembership, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresMembershipStore_ListForUser(t *testing.T) {
	s, mock := newMembershipStore(t)
	ctx := context.Background()
	uid := uuid.New()

	mock.ExpectQuery(`FROM auth.project_membership`).WithArgs(uid).
		WillReturnRows(sqlmock.NewRows([]string{"project", "role"}).
			AddRow("proj-a", ProjectRoleViewer).
			AddRow("proj-b", ProjectRoleContributor))
	out, err := s.ListForUser(ctx, uid)
	if err != nil || len(out) != 2 || out[1].Role != ProjectRoleContributor {
		t.Fatalf("ListForUser = %+v, %v", out, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresMembershipStore_GrantAndRevoke(t *testing.T) {
	s, mock := newMembershipStore(t)
	ctx := context.Background()
	uid := uuid.New()

	// createdBy "" ⇒ NULL (IdP-sync row).
	mock.ExpectExec(`INSERT INTO auth.project_membership`).
		WithArgs(uid, "proj-1", ProjectRoleViewer, sql.NullString{}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := s.Grant(ctx, uid, "proj-1", ProjectRoleViewer, ""); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	mock.ExpectExec(`DELETE FROM auth.project_membership`).WithArgs(uid, "proj-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := s.Revoke(ctx, uid, "proj-1"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
