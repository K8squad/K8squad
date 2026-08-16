package events

import "strings"

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
