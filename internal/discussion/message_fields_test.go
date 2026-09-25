package discussion

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMessageAudienceKindFields(t *testing.T) {
	// Test that Message struct properly includes audience, kind, and payload fields
	msg := Message{
		ID:        uuid.New(),
		ThreadID:  uuid.New(),
		Body:      "test message",
		Audience:  "party",
		Kind:      "text",
		CreatedAt: time.Now(),
	}

	if msg.Audience != "party" {
		t.Errorf("expected audience 'party', got %s", msg.Audience)
	}
	if msg.Kind != "text" {
		t.Errorf("expected kind 'text', got %s", msg.Kind)
	}
	if msg.Payload != nil {
		t.Error("expected payload to be nil for default message")
	}
}

func TestPostMessageWithDirectAudience(t *testing.T) {
	// Validation-level coverage for the wire fields (no DB required): the store's
	// normalizeAudience / normalizeKind enforce the same contract as migration 0024's
	// CHECK constraints, so a bad value fails with a 400-mapped sentinel, not a 500.

	testCases := []struct {
		name        string
		audience    string
		kind        string
		payload     *json.RawMessage
		shouldError bool
	}{
		{
			name:        "default party message",
			audience:    "",
			kind:        "",
			payload:     nil,
			shouldError: false,
		},
		{
			name:        "party message",
			audience:    "party",
			kind:        "text",
			payload:     nil,
			shouldError: false,
		},
		{
			name:        "direct message",
			audience:    "direct:test-agent-id",
			kind:        "text",
			payload:     nil,
			shouldError: false,
		},
		{
			name:        "invalid audience",
			audience:    "invalid",
			kind:        "text",
			payload:     nil,
			shouldError: true,
		},
		{
			name:        "empty direct target rejected",
			audience:    "direct:",
			kind:        "text",
			payload:     nil,
			shouldError: true,
		},
		{
			name:        "valid structured kind",
			audience:    "party",
			kind:        "structured",
			payload:     nil,
			shouldError: false,
		},
		{
			name:        "invalid kind",
			audience:    "party",
			kind:        "invalid",
			payload:     nil,
			shouldError: true,
		},
		{
			name:     "message with payload",
			audience: "party",
			kind:     "structured",
			payload: func() *json.RawMessage {
				var p json.RawMessage = json.RawMessage(`{"key": "value"}`)
				return &p
			}(),
			shouldError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			audience, kind := tc.audience, tc.kind
			a, aerr := normalizeAudience(&audience)
			k, kerr := normalizeKind(&kind)
			if tc.shouldError {
				if aerr == nil && kerr == nil {
					t.Fatalf("expected validation error, got audience=%q kind=%q", a, k)
				}
				return
			}
			if aerr != nil {
				t.Fatalf("unexpected audience error: %v", aerr)
			}
			if kerr != nil {
				t.Fatalf("unexpected kind error: %v", kerr)
			}
			// Defaults: absent audience ⇒ party, absent kind ⇒ text.
			if audience == "" && a != "party" {
				t.Errorf("default audience: want party, got %q", a)
			}
			if kind == "" && k != "text" {
				t.Errorf("default kind: want text, got %q", k)
			}
		})
	}
}

func TestVisibilityPredicate(t *testing.T) {
	// Test the visibility logic for different audiences
	// This would be tested with actual database queries in integration tests

	// Mock author context
	auth := AuthorContext{
		Principal: "test-user",
		IsAdmin:   false,
	}

	// Test cases for visibility logic
	testCases := []struct {
		name      string
		audience  string
		author    string
		shouldSee bool
	}{
		{
			name:      "party messages are always visible",
			audience:  "party",
			author:    "other-user",
			shouldSee: true,
		},
		{
			name:      "own direct messages are visible",
			audience:  "direct:test-user",
			author:    "test-user",
			shouldSee: true,
		},
		{
			name:      "direct messages to others are not visible",
			audience:  "direct:other-user",
			author:    "other-user",
			shouldSee: false,
		},
		{
			name:      "admin can see all messages",
			audience:  "direct:third-user",
			author:    "third-user",
			shouldSee: false, // Would be true if IsAdmin: true
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// This would contain the actual visibility logic
			// For now, just demonstrate the test structure

			// In a real test, this would query the database and check results
			_ = auth // Use the auth context
			_ = tc.audience
			_ = tc.author
		})
	}
}
