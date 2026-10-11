// ISI-5672 (ISI-5598 WS-B, ADR-0030 Option 2 "reset-marker") — the /reset backend.
//
// The discussion schema is append-only with one-way soft-retract and NO hard delete (0004 triggers), so
// "reset" cannot mean "delete the rows". ADR-0030 defines it as a reset-MARKER: /reset appends exactly
// one server-stamped kind='reset' message; the reader (GetThread / ListThreads, store.go) renders only
// the live, non-marker messages created STRICTLY after the latest live marker (the reset floor). The
// marker is itself soft-retractable, so retracting it is the undo-reset path — no new write path.
//
// This file is the dedicated write surface (ResetThread) + the human-only guard (ResetAllowed) + the
// read-back of the reset metadata (loadResetMarker). It deliberately does NOT route through the generic
// PostMessage/normalizeKind path (which rejects kind='reset'): reset is stamped ONLY here, via the
// human-only POST .../threads/{id}/reset endpoint, exactly as party_start posts only through
// StartPartySession (ADR-0030 §(a)6). This keeps kind='reset' off the agent tool surface and makes a
// client-forged reset un-representable.
package discussion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// ErrResetForbidden — only a human may reset a discussion (ADR-0030 §(a)7). An agent cannot wipe a
// thread; keeping reset off the agent tool surface preserves the anti-runaway posture party_start
// established. Agent-vs-human is DERIVED from the server-stamped provenance (auth.AgentID), never a body
// flag. Maps to 403 (writeStoreErr).
var ErrResetForbidden = errors.New("discussion: only a human may reset a discussion")

// ResetAllowed is the human-only guard for /reset (ADR-0030 §(a)7), the exact mirror of PartyStartAllowed.
func ResetAllowed(auth AuthorContext) error {
	if auth.AgentID != nil {
		return ErrResetForbidden
	}
	return nil
}

// resetMarkerBody is the system body stamped on a reset marker. It is never rendered as a chat bubble
// (the reader excludes kind='reset'); it exists only so the append-only row carries a non-empty,
// self-describing body for the audit trail / memory index.
const resetMarkerBody = "— discussion cleared —"

// resetMarkerPayload is the kind='reset' marker's structured payload (ADR-0030 §4.2): the optional
// human-supplied reason plus the count of messages this reset hid, a server-stamped audit nicety.
type resetMarkerPayload struct {
	Reason      string `json:"reason,omitempty"`
	HiddenCount int    `json:"hiddenCount"`
}

// ResetThread clears the visible transcript of a thread per ADR-0030 (Option 2): in ONE transaction it
// (1) closes any active party-session in the thread (guarded so WS-B/ISI-5585 land in either order) and
// (2) appends a server-stamped, human-authored kind='reset' marker. The marker's created_at becomes the
// new reset floor; GetThread/ListThreads then hide every earlier message. Human-only (ErrResetForbidden
// ⇒ 403). Returns the committed marker + how many messages it hid. Nothing is deleted — the hidden
// transcript stays in the DB (append-only / compliance) and reappears if the marker is retracted.
func (s *Store) ResetThread(ctx context.Context, projectID string, teamID, threadID uuid.UUID, auth AuthorContext, reason string) (*Message, int, error) {
	// Human-only (ADR-0030 §(a)7) — checked BEFORE any DB work so an agent caller is rejected cleanly.
	if err := ResetAllowed(auth); err != nil {
		return nil, 0, err
	}
	if err := s.assertThreadInScope(ctx, projectID, teamID, threadID); err != nil {
		return nil, 0, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// (b) Atomically CLOSE any active party-session in the thread. Leaving a live advancer pointed at a
	// now-hidden topic would keep minting paid Runs against an invisible debate (ADR-0027 cost-control
	// hazard), so the close is mandatory. Guarded by to_regclass so WS-B and ISI-5585 (migration 0033)
	// can land in either order — the guard is a permanent, harmless no-op when the table is absent. The
	// halt is logged non-silently (ADR-0027 §5.1 convention: dropped/halted counts are never silent).
	var partyTbl sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('discussion.party_session')::text`).Scan(&partyTbl); err != nil {
		return nil, 0, fmt.Errorf("reset: probe party_session: %w", err)
	}
	if partyTbl.Valid {
		tag, err := tx.ExecContext(ctx, `
			UPDATE discussion.party_session
			   SET phase = 'closed', closed_at = now()
			 WHERE thread_id = $1 AND phase = 'active'`, threadID)
		if err != nil {
			return nil, 0, fmt.Errorf("reset: close active party session: %w", err)
		}
		if n, _ := tag.RowsAffected(); n > 0 {
			slog.InfoContext(ctx, "discussion: /reset closed active party session",
				"threadID", threadID, "closed", n, "principal", auth.Principal)
		}
	}

	// Count the messages this reset will hide = the live, non-marker messages above the PRIOR reset
	// floor (exactly what the reader currently shows). Recorded in the marker payload (§4.2).
	var hiddenCount int
	if err := tx.QueryRowContext(ctx, `
		WITH r AS (
		    SELECT COALESCE(MAX(created_at), '-infinity'::timestamptz) AS reset_at
		    FROM discussion.message
		    WHERE thread_id = $1 AND kind = 'reset' AND invalidated_at IS NULL
		)
		SELECT count(*)
		FROM discussion.message, r
		WHERE thread_id = $1 AND invalidated_at IS NULL AND kind <> 'reset' AND created_at > r.reset_at`,
		threadID).Scan(&hiddenCount); err != nil {
		return nil, 0, fmt.Errorf("reset: count hidden: %w", err)
	}

	payloadBytes, err := json.Marshal(resetMarkerPayload{Reason: reason, HiddenCount: hiddenCount})
	if err != nil {
		return nil, 0, fmt.Errorf("reset: marshal payload: %w", err)
	}
	payload := json.RawMessage(payloadBytes)
	// Server-stamped via insertMessage with kind="reset" — author_agent_id is NULL by construction
	// (ResetAllowed rejected any agent caller), honouring the §(a)7 human-only fence.
	marker, err := insertMessage(ctx, tx, threadID, nil, auth, resetMarkerBody, "party", "reset", &payload)
	if err != nil {
		return nil, 0, fmt.Errorf("reset: insert marker: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return marker, hiddenCount, nil
}

// loadResetMarker populates a thread's reset metadata (ResetAt/ResetBy/ResetReason) from the latest LIVE
// kind='reset' marker (ADR-0030 §(c)), so GetThread exposes "cleared {T} by {who}" without a refetch. No
// live marker (never reset, or the last reset was retracted) leaves the fields nil — the full-transcript
// read. The kind column predates 0036 (it exists since 0024), so this is valid even pre-reset-migration.
func (s *Store) loadResetMarker(ctx context.Context, threadID uuid.UUID, t *Thread) error {
	var createdAt time.Time
	var by string
	var payload sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT created_at, author_principal, payload
		FROM discussion.message
		WHERE thread_id = $1 AND kind = 'reset' AND invalidated_at IS NULL
		ORDER BY created_at DESC
		LIMIT 1`, threadID).Scan(&createdAt, &by, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	t.ResetAt = &createdAt
	t.ResetBy = &by
	if payload.Valid {
		var p resetMarkerPayload
		if json.Unmarshal([]byte(payload.String), &p) == nil && p.Reason != "" {
			reason := p.Reason
			t.ResetReason = &reason
		}
	}
	return nil
}
