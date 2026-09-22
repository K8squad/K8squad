// reviewitem_unit_test.go — the NO-Postgres unit lane for EnsureReviewWorkItem
// (ISI-4776): the fail-closed input guards that return BEFORE any BeginTx. The
// DB-backed properties — the advisory-lock create-if-absent race (two concurrent
// ensures ⇒ exactly one insert), the label lookup, and the audit co-commit —
// require a live Postgres and are exercised in the coord chaos/integration lane;
// here we pin only the branches that never touch the DB so every `go test ./...`
// re-proves them. Mirrors workitemwrite_unit_test.go.
package coord

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestEnsureReviewWorkItemRejectsBadInput(t *testing.T) {
	s := newOfflineWriteStore(t)
	base := EnsureReviewWorkItemInput{
		ProjectID:  "proj",
		Title:      "Review acme/widget#42",
		Principal:  "system:review-automation",
		DedupLabel: "ksquad.review=deadbeef",
	}
	cases := []struct {
		name string
		mut  func(*EnsureReviewWorkItemInput)
	}{
		{"empty projectID", func(in *EnsureReviewWorkItemInput) { in.ProjectID = "" }},
		{"empty title", func(in *EnsureReviewWorkItemInput) { in.Title = "" }},
		{"empty principal", func(in *EnsureReviewWorkItemInput) { in.Principal = "" }},
		{"empty dedup label", func(in *EnsureReviewWorkItemInput) { in.DedupLabel = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mut(&in)
			if _, err := s.EnsureReviewWorkItem(context.Background(), in); !errors.Is(err, ErrInvalidWorkItem) {
				t.Fatalf("want ErrInvalidWorkItem, got %v", err)
			}
		})
	}
}

// TestEnsureReviewWorkItemRejectsUnnormalizableLabel: a dedup label the create-
// field normalizer would drop (oversized) fails closed BEFORE the txn rather than
// inserting an item a later lookup could never find.
func TestEnsureReviewWorkItemRejectsUnnormalizableLabel(t *testing.T) {
	s := newOfflineWriteStore(t)
	in := EnsureReviewWorkItemInput{
		ProjectID:  "proj",
		Title:      "Review",
		Principal:  "system:review-automation",
		DedupLabel: "ksquad.review=" + strings.Repeat("x", maxLabelLen), // > maxLabelLen total
	}
	if _, err := s.EnsureReviewWorkItem(context.Background(), in); !errors.Is(err, ErrInvalidWorkItem) {
		t.Fatalf("want ErrInvalidWorkItem for an oversized label, got %v", err)
	}
}
