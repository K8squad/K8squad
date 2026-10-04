package apiserver

// teamhomens.go — the ONE Team-UID → HOME-namespace resolver (ISI-5422).
//
// History: the UID→namespace resolution grew as ~6 near-identical copies
// (fleetlist, projectresolve, credentials, composecrd, secretwrite, the two
// probe services) and they drifted exactly the way drift always ends —
// silently, and against each other: the read models matched the Team CR's
// metadata (HOME) namespace while the write models returned
// Team.Status.Namespace (the EXEC namespace the operator provisions for Run
// CRs, ISI-4128), so a compose write landed its CR where no read would ever
// find it (ISI-5415, fixed by PR #765). This file is the dedup nit from that
// review (ISI-5418 nit #3): one match core, one fail-closed ns extraction,
// and per-caller adapters that only translate error vocabulary.
//
// The contract every caller now shares:
//   - match the Team CR by UID (immutable for the object's lifetime, so a
//     rename can never widen a scope and a name collision can never cross
//     tenancy);
//   - the HOME namespace is Team.Namespace — where the Team/Project/Agent/
//     Role/Skill CRs and the credential Secrets live, and where every read
//     model already looks;
//   - Status.Namespace is NEVER the answer here. It gates "a provisioned
//     squad" in the fleet-admin selection (fleetAdminTeam, credfleet.go) and
//     the credential probe's provisioned-team check — nothing else;
//   - fail closed: unknown UID or an empty home ns is
//     ErrTeamNamespaceUnresolved — never a fallback to a shared namespace.
//
// Error vocabulary: the shared core speaks ErrTeamNamespaceUnresolved (the
// write-path/404 vocabulary). Read models whose handlers answer ErrTeamNotFound
// map through their thin adapters (resolveTeamNamespace in projectresolve.go,
// ClientFleetListReader.teamNamespace, ClientCredentialReader.teamNamespace) —
// the mapping is the ONLY thing those adapters add.

import (
	"context"
	"errors"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// ErrTeamNamespaceUnresolved is returned when the caller's Team UID resolves
// to no Team, or to a Team with no home namespace. Handlers answer 404 — a
// caller with no Team scope has nowhere to read or write. (Lives here since
// ISI-5422; previously defined next to ComposeService.teamNamespace.)
var ErrTeamNamespaceUnresolved = errors.New("apiserver: caller team namespace unresolved")

// teamLister is the narrow List seam the shared resolver needs. client.Reader
// (the host's informer cache, a fake in tests) satisfies it structurally, and
// so do the write services' minimal client seams (SecretWriteClient) — the
// write paths can delegate to the shared core without widening their own
// interfaces or importing anything new.
type teamLister interface {
	List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error
}

// matchTeamByUID is the single UID-match core: list the Team CRs through the
// caller's reader (the informer cache in the host, a fake in tests) and
// return the Team whose UID equals teamUID. Empty UID or no match is
// ErrTeamNamespaceUnresolved — existence-hiding, indistinguishable either way.
// The returned pointer aliases the List result; callers that need to Update
// the CR (the probes' annotation writes) do so within the same request, so
// the alias is never retained past the response.
func matchTeamByUID(ctx context.Context, lister teamLister, teamUID string) (*ksquadv1.Team, error) {
	if teamUID == "" {
		return nil, ErrTeamNamespaceUnresolved
	}
	var teams ksquadv1.TeamList
	if err := lister.List(ctx, &teams); err != nil {
		return nil, err
	}
	for i := range teams.Items {
		if string(teams.Items[i].UID) == teamUID {
			return &teams.Items[i], nil
		}
	}
	return nil, ErrTeamNamespaceUnresolved
}

// teamHomeNamespace extracts a Team's HOME namespace (the CR's own metadata
// namespace), fail-closed: empty ns ⇒ ErrTeamNamespaceUnresolved. A Team CR
// cannot exist outside its (existing) home namespace in a real cluster, so
// the guard only keeps a mis-seeded fake/dev host from reading or writing a
// namespace-less coordinate. Shared by the bound-caller resolver below and
// the fleet-admin target extraction, so both fail identically.
func teamHomeNamespace(team *ksquadv1.Team) (string, error) {
	if ns := team.Namespace; ns != "" {
		return ns, nil
	}
	return "", ErrTeamNamespaceUnresolved
}

// resolveTeamHomeNamespace is THE Team-UID → HOME-ns resolver for read and
// write paths alike (ISI-5422): match the Team by UID, return its metadata
// namespace. Unknown UID or empty ns fails closed with
// ErrTeamNamespaceUnresolved. Callers whose handlers answer a different
// vocabulary (ErrTeamNotFound) wrap this and map the sentinel — they must not
// re-implement the match.
func resolveTeamHomeNamespace(ctx context.Context, lister teamLister, teamUID string) (string, error) {
	team, err := matchTeamByUID(ctx, lister, teamUID)
	if err != nil {
		return "", err
	}
	return teamHomeNamespace(team)
}

// errTeamNotFoundFrom maps the shared core's ErrTeamNamespaceUnresolved onto
// the read models' ErrTeamNotFound vocabulary (the dashboard/settings/fleet
// read surfaces answer 404 through that sentinel, overview.go). Transport
// errors pass through untouched. This is the ONLY permitted translation —
// adapters calling it must not also re-match the Team.
func errTeamNotFoundFrom(err error) error {
	if errors.Is(err, ErrTeamNamespaceUnresolved) {
		return ErrTeamNotFound
	}
	return err
}
