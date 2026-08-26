package events

import (
	"fmt"
	"strings"
)

// DefaultPrefix is the root token of every event subject (relay.subjectPrefix in
// the Story 9.4 event-relay ConfigMap). Plugins subscribe with wildcards below
// it, e.g. `ksquad.run.>` or `ksquad.*.<project>.>`.
const DefaultPrefix = "ksquad"

// nullSquadToken stands in for a NULL squad (work_item.team_id is nullable,
// §6.1) so the subject keeps a fixed five-token shape and wildcard subscriptions
// line up positionally. NATS treats it as an ordinary literal token.
const nullSquadToken = "_"

// Subject composes the JetStream subject `{prefix}.{entity}.{project}.{squad}.{event_type}`
// from the outbox COLUMNS — never from the payload (§17.4). An empty squad
// becomes the "_" token so the taxonomy stays positional. Each component is
// sanitized of the NATS token separators/wildcards (`.`, ` `, `*`, `>`) that
// would otherwise split or widen the subject; uuids and the catalog event types
// contain none, so this only ever guards against a malformed value.
func Subject(prefix, entity, project, squad, eventType string) string {
	if prefix == "" {
		prefix = DefaultPrefix
	}
	if squad == "" {
		squad = nullSquadToken
	}
	return strings.Join([]string{
		sanitizeToken(prefix),
		sanitizeToken(entity),
		sanitizeToken(project),
		sanitizeToken(squad),
		sanitizeToken(eventType),
	}, ".")
}

// SubjectParts is the taxonomy a subscriber recovers from a delivered subject.
// It is the read-side mirror of the four subject components the relay composes
// from the outbox COLUMNS — the payload is NEVER parsed for these (§17.4), so a
// plugin can route on entity/project/squad/event_type without unmarshalling the
// (versioned) body first.
type SubjectParts struct {
	Prefix    string // subject root ("ksquad")
	Entity    string // one of Entities
	ProjectID string // tenancy predicate + subject component
	Squad     string // team_id; "" when the subject carried the "_" NULL token
	EventType string // e.g. completed|claimed|handoff
}

// ParseSubject splits a delivered five-token subject back into its taxonomy for
// the plugin subscribe SDK (Story 12.2). It is the inverse of Subject for real
// values: the relay guarantees exactly five tokens (each component is sanitized
// of the '.' separator before publish), so anything else is a malformed/foreign
// subject and is rejected rather than silently mis-routed. The "_" squad token
// decodes back to "" to mirror Event.Squad's NULL semantics — a real squad is a
// team uuid and is never literally "_".
func ParseSubject(subject string) (SubjectParts, error) {
	tok := strings.Split(subject, ".")
	if len(tok) != 5 {
		return SubjectParts{}, fmt.Errorf("events.ParseSubject: %q is not a 5-token subject (prefix.entity.project.squad.event_type)", subject)
	}
	squad := tok[3]
	if squad == nullSquadToken {
		squad = ""
	}
	return SubjectParts{
		Prefix:    tok[0],
		Entity:    tok[1],
		ProjectID: tok[2],
		Squad:     squad,
		EventType: tok[4],
	}, nil
}

// sanitizeToken replaces NATS subject metacharacters with '_' so a single
// component can never split into multiple tokens or turn into a wildcard.
func sanitizeToken(tok string) string {
	if tok == "" {
		return nullSquadToken
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '.', ' ', '\t', '*', '>':
			return '_'
		default:
			return r
		}
	}, tok)
}
