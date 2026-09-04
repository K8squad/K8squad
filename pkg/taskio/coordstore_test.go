package taskio

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"
)

func TestProjectDetailMapsAllFields(t *testing.T) {
	ts := time.Unix(1700000000, 0).UTC()
	in := coord.TaskDetail{
		WorkItemID:         "wi-1",
		Title:              "S2",
		Description:        "seam",
		State:              "in_progress",
		BlockedReason:      "needs_approval",
		AcceptanceCriteria: []string{"ac1"},
		Goals:              []string{"g1"},
		FenceToken:         7,
		Holder:             "agent",
		RunID:              "run-A",
		Comments:           []coord.TaskComment{{Author: "agent", Body: "hi", CreatedAt: ts}},
	}
	got := projectDetail(in)
	if got.WorkItemID != "wi-1" || got.Title != "S2" || got.Description != "seam" ||
		got.State != "in_progress" || got.BlockedReason != "needs_approval" ||
		got.FenceToken != 7 || got.Holder != "agent" || got.RunID != "run-A" {
		t.Fatalf("scalar fields mismatched: %+v", got)
	}
	if len(got.AcceptanceCriteria) != 1 || got.AcceptanceCriteria[0] != "ac1" ||
		len(got.Goals) != 1 || got.Goals[0] != "g1" {
		t.Fatalf("ac/goals mismatched: %+v", got)
	}
	if len(got.Comments) != 1 || got.Comments[0].Author != "agent" || got.Comments[0].Body != "hi" ||
		!got.Comments[0].CreatedAt.Equal(ts) {
		t.Fatalf("comments mismatched: %+v", got.Comments)
	}
}

// Comments must render as [] (non-nil), never null, when there are none.
func TestProjectDetailCommentsNeverNil(t *testing.T) {
	got := projectDetail(coord.TaskDetail{WorkItemID: "wi-1"})
	if got.Comments == nil {
		t.Fatalf("Comments is nil; want empty non-nil slice")
	}
	if len(got.Comments) != 0 {
		t.Fatalf("Comments = %v, want empty", got.Comments)
	}
}

func TestMapCoordErr(t *testing.T) {
	other := errors.New("boom")
	cases := []struct {
		in   error
		want error
	}{
		{coord.ErrWorkItemNotFound, ErrNotFound},
		{fmt.Errorf("wrap: %w", coord.ErrWorkItemNotFound), ErrNotFound},
		{coord.ErrInvalidState, ErrInvalidTransition},
		{coord.ErrStateConflict, ErrInvalidTransition},
		{other, other},
		{nil, nil},
	}
	for _, c := range cases {
		got := mapCoordErr(c.in)
		if c.want == nil {
			if got != nil {
				t.Errorf("mapCoordErr(nil) = %v, want nil", got)
			}
			continue
		}
		if !errors.Is(got, c.want) {
			t.Errorf("mapCoordErr(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestNewCoordStoreRequiresDeps(t *testing.T) {
	if _, err := NewCoordStore(nil, nil); err == nil {
		t.Fatalf("NewCoordStore(nil,nil) should error")
	}
}
