// ticketkey_test.go — the NO-Postgres unit lane for the canonical ticket-key
// label convention (ISI-6088): the builder, its survival through the create-field
// normalizer, and the fail-closed stamp guards on CreateWorkItem that return
// BEFORE any BeginTx. Mirrors workitemwrite_unit_test.go.
package coord

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestTicketKeyLabel — the canonical label is prefix + key, and coord's prefix is
// the same literal rundrive's resolver reads (the writer/reader drift guard is
// TestResolveTicketKey in pkg/controller/rundrive, which resolves this label).
func TestTicketKeyLabel(t *testing.T) {
	if got := TicketKeyLabel("ISI-6088"); got != "ksquad.ticket.key=ISI-6088" {
		t.Fatalf("TicketKeyLabel = %q, want ksquad.ticket.key=ISI-6088", got)
	}
	if TicketKeyLabelPrefix != "ksquad.ticket.key=" {
		t.Fatalf("TicketKeyLabelPrefix = %q, want ksquad.ticket.key=", TicketKeyLabelPrefix)
	}
}

// TestTicketKeyLabelSurvivesNormalization — a stamped ticket-key label comes out
// of normalizeCreateFields first, unchanged, deduped against an identical
// caller-passed label, so it can never be squeezed out of the persisted set.
func TestTicketKeyLabelSurvivesNormalization(t *testing.T) {
	_, _, labels, err := normalizeCreateFields("", "", []string{
		TicketKeyLabel("ISI-6088"), "bug", TicketKeyLabel("ISI-6088"),
	})
	if err != nil {
		t.Fatalf("normalizeCreateFields: %v", err)
	}
	want := []string{TicketKeyLabel("ISI-6088"), "bug"}
	if len(labels) != len(want) || labels[0] != want[0] || labels[1] != want[1] {
		t.Fatalf("labels = %+v, want %+v", labels, want)
	}
}

// TestCreateWorkItemTicketKeyGuards (ISI-6088) — the ticket-key stamp fails
// closed BEFORE the txn (offline DB proves no BeginTx was reached): a key that
// conflicts with a DIFFERENT caller-passed ksquad.ticket.key= label is a
// rejected double stamp, and an over-long key (the stamped label overruns
// maxLabelLen) is rejected by the create-field bound. A key matching an
// existing label, in contrast, passes the guard cleanly (it then fails on the
// offline DB's begin — a non-ErrInvalidWorkItem error — proving the guard let
// the de-dup through).
func TestCreateWorkItemTicketKeyGuards(t *testing.T) {
	s := newOfflineWriteStore(t)
	base := CreateWorkItemInput{ProjectID: "p", Title: "t", Principal: "user:a", TicketKey: "ISI-6088"}

	conflict := base
	conflict.Labels = []string{"ksquad.ticket.key=ISI-9999"}
	if _, err := s.CreateWorkItem(context.Background(), conflict); !errors.Is(err, ErrInvalidWorkItem) {
		t.Fatalf("conflicting double stamp: want ErrInvalidWorkItem, got %v", err)
	}

	// Padded conflict (CR ISI-6121): the guard reads labels TRIMMED, so a
	// whitespace-padded key label hits the same 400, never slips past the guard
	// to persist as a stray second key label.
	padded := base
	padded.Labels = []string{" ksquad.ticket.key=ISI-9999 "}
	if _, err := s.CreateWorkItem(context.Background(), padded); !errors.Is(err, ErrInvalidWorkItem) {
		t.Fatalf("padded conflicting double stamp: want ErrInvalidWorkItem, got %v", err)
	}

	overlong := base
	overlong.TicketKey = strings.Repeat("k", maxLabelLen) // stamped label > maxLabelLen
	if _, err := s.CreateWorkItem(context.Background(), overlong); !errors.Is(err, ErrInvalidWorkItem) {
		t.Fatalf("over-long ticketKey: want ErrInvalidWorkItem, got %v", err)
	}

	dup := base
	dup.Labels = []string{"ksquad.ticket.key=ISI-6088"} // same value — clean de-dup, not a conflict
	_, err := s.CreateWorkItem(context.Background(), dup)
	if err == nil {
		t.Fatal("offline DB: expected a begin failure, got nil")
	}
	if errors.Is(err, ErrInvalidWorkItem) {
		t.Fatalf("matching key must pass the guard (fail later on the offline DB), got %v", err)
	}
}
