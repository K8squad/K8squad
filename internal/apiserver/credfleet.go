package apiserver

import (
	"errors"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// ============================================================================
// Fleet-wide-admin credential targeting (ISI-3937, extends ISI-3932 / ADR-039)
// ============================================================================
//
// ISI-3932 made the READ models (overview.go / org.go / onboarding.go) fleet-wide
// for a global admin, but left the credential WRITE / TEST / LIST surface strictly
// caller-team-scoped — so the bootstrap admin, whose team_id backs no Team CR
// (ISI-3921, Option-b inert), 404s on every credential op with no shared-namespace
// fallback to lean on (the deliberate tenancy invariant).
//
// A fleet-wide admin has no team of its own, so credential ops must operate against
// an EXPLICITLY targeted team from the fleet, resolved through the SAME Team-CR-uid +
// status.namespace discipline every other credential path uses. This helper is the
// one place that selection lives; each service (secretwrite / credentialtest /
// credentials) calls it only AFTER its own caller-team resolution has failed AND the
// caller is an admin, then extracts the namespace in its own convention. That
// ordering is the guardrail: a caller whose own team resolves — bound admin or
// non-admin — never reaches here, so an already-bound non-admin can never target a
// foreign team (no tenancy hijack), and requestedTeamID is inert on the own-team path.

// ErrSelectTeam signals that a fleet-wide admin must name a target team: more than one
// reconciled team exists on the install and the request supplied no teamId. Handlers
// answer 400 (a picker prompt) — NOT 404 — because the surface exists and the caller
// simply must choose which team to target.
var ErrSelectTeam = errors.New("apiserver: fleet admin must select a target team")

// fleetAdminTeam picks the Team a fleet-wide admin's credential op targets, once the
// admin's OWN team has proven unresolvable (ISI-3921). Only teams the reconciler has
// stamped a status.namespace on are targetable (a real, provisioned squad namespace):
//
//   - requestedTeamID names a reconciled Team → that team.
//   - no requestedTeamID, exactly one reconciled team on the install → that team (the
//     common single-squad case, e.g. isitobservable — no picker needed).
//   - no requestedTeamID, more than one reconciled team → ErrSelectTeam (400).
//   - requestedTeamID names nothing reconciled, or zero teams exist →
//     ErrTeamNamespaceUnresolved (404, existence-hiding: a foreign/absent/un-reconciled
//     UID is indistinguishable, never an oracle and never a shared-namespace fallback).
func fleetAdminTeam(teams []ksquadv1.Team, requestedTeamID string) (*ksquadv1.Team, error) {
	reconciled := make([]*ksquadv1.Team, 0, len(teams))
	for i := range teams {
		if teams[i].Status.Namespace != "" {
			reconciled = append(reconciled, &teams[i])
		}
	}
	if requestedTeamID != "" {
		for _, t := range reconciled {
			if string(t.UID) == requestedTeamID {
				return t, nil
			}
		}
		return nil, ErrTeamNamespaceUnresolved
	}
	switch len(reconciled) {
	case 1:
		return reconciled[0], nil
	case 0:
		return nil, ErrTeamNamespaceUnresolved
	default:
		return nil, ErrSelectTeam
	}
}
