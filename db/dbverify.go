package dbmigrate

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
)

// managedSchemas lists the top-level schemas this runner owns (see the package doc). Verify
// asserts the standing ownership invariant over exactly these.
var managedSchemas = []string{"auth", "coord", "discussion", "scm"}

// Drift describes one managed-schema table that breaks the standing ownership/DML invariant
// (ISI-6371): either its owner differs from the majority owner of its schema, or the connecting
// role is missing DML privileges on it.
type Drift struct {
	Schema            string
	Table             string
	Owner             string
	IntendedOwner     string // majority owner of the schema; "" when none applies
	MissingPrivileges []string
	Ambiguous         bool // schema's owner counts are tied — direction is undecidable
}

// String renders one drift line for the log, carrying the runbook pointer the SQL WARNING used
// to carry (notices never reach the driver logs, so this is the operator-facing signal).
func (d Drift) String() string {
	missing := strings.Join(d.MissingPrivileges, "+")
	switch {
	case d.Ambiguous:
		return fmt.Sprintf("%s.%s: owners are tied (%s vs %s) — ownership direction is ambiguous, refusing to reconcile; resolve by hand", d.Schema, d.Table, d.Owner, d.IntendedOwner)
	case d.Owner != d.IntendedOwner && missing != "":
		return fmt.Sprintf("%s.%s: owner %s != majority owner %s and current role lacks %s (SQLSTATE 42501 class; run db/migrations/0041_discussion_proposal_privileges_reconcile.sql once as a superuser)", d.Schema, d.Table, d.Owner, d.IntendedOwner, missing)
	case d.Owner != d.IntendedOwner:
		return fmt.Sprintf("%s.%s: owner %s != majority owner %s (ISI-6371 drift class; run db/migrations/0041_discussion_proposal_privileges_reconcile.sql once as a superuser)", d.Schema, d.Table, d.Owner, d.IntendedOwner)
	default:
		return fmt.Sprintf("%s.%s: owner %s, but current role lacks %s (SQLSTATE 42501 class)", d.Schema, d.Table, d.Owner, missing)
	}
}

// Verify asserts the standing invariant the 0041 reconcile migration established — every table
// in every managed schema is owned by that schema's majority owner AND the connecting role holds
// full DML on it (ISI-6371). It runs on EVERY boot, independent of the migration ledger: Apply
// is one-shot per version (the ledger short-circuits already-applied files), so drift that lands
// after 0041 applied — or on a DB where 0041 already ran — is invisible to the migration itself.
//
// It is advisory by design: drift is returned for the caller to log loudly, never fatal, so a
// drifted-but-serviceable store still boots (Apply remains the fail-closed path). SQL WARNING
// notices cannot carry this signal because database/sql+pgx drops them unless a NoticeHandler is
// wired (the apiserver wires one).
func Verify(ctx context.Context, db *sql.DB) ([]Drift, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT schemaname, tablename, tableowner,
		       has_table_privilege(current_user, format('%I.%I', schemaname, tablename), 'SELECT'),
		       has_table_privilege(current_user, format('%I.%I', schemaname, tablename), 'INSERT'),
		       has_table_privilege(current_user, format('%I.%I', schemaname, tablename), 'UPDATE'),
		       has_table_privilege(current_user, format('%I.%I', schemaname, tablename), 'DELETE')
		FROM pg_tables
		WHERE schemaname = ANY($1)
		ORDER BY schemaname, tablename`, managedSchemas)
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: verify: %w", err)
	}
	defer rows.Close()

	type row struct {
		schema, table, owner string
		sel, ins, upd, del   bool
	}
	bySchema := map[string][]row{}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.schema, &r.table, &r.owner, &r.sel, &r.ins, &r.upd, &r.del); err != nil {
			return nil, fmt.Errorf("dbmigrate: verify scan: %w", err)
		}
		bySchema[r.schema] = append(bySchema[r.schema], r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dbmigrate: verify: %w", err)
	}

	var drifts []Drift
	for _, schema := range managedSchemas {
		rs, ok := bySchema[schema]
		if !ok || len(rs) == 0 {
			continue // schema absent (partial-schema lanes)
		}
		counts := map[string]int{}
		for _, r := range rs {
			counts[r.owner]++
		}
		// Majority owner, resolved deterministically (count DESC, then name ASC) so repeated
		// runs are stable. A tie is ambiguous: reported, never repaired (same rule as 0041).
		intended, max := "", -1
		for o, c := range counts {
			if c > max || (c == max && o < intended) {
				intended, max = o, c
			}
		}
		tied := false
		for o, c := range counts {
			if o != intended && c == max {
				tied = true
			}
		}
		for _, r := range rs {
			d := Drift{Schema: r.schema, Table: r.table, Owner: r.owner, IntendedOwner: intended, Ambiguous: tied}
			collectMissing(&d, r.sel, r.ins, r.upd, r.del)
			if tied || r.owner != intended || len(d.MissingPrivileges) > 0 {
				drifts = append(drifts, d)
			}
		}
	}
	return drifts, nil
}

// VerifyAndLog runs Verify and reports through the Go logger — the every-boot operator signal
// for the ISI-6371 drift class (a healthy boot logs one confirmation line, a drifted boot names
// every drifted table with the runbook pointer).
func VerifyAndLog(ctx context.Context, db *sql.DB) {
	drifts, err := Verify(ctx, db)
	if err != nil {
		log.Printf("dbmigrate: ownership verify failed: %v", err)
		return
	}
	if len(drifts) == 0 {
		log.Printf("dbmigrate: ownership invariant verified across %s (each schema single-owner, full DML held)", strings.Join(managedSchemas, ", "))
		return
	}
	for _, d := range drifts {
		log.Printf("dbmigrate: DRIFT %s", d)
	}
}

func collectMissing(d *Drift, sel, ins, upd, del bool) {
	if !sel {
		d.MissingPrivileges = append(d.MissingPrivileges, "SELECT")
	}
	if !ins {
		d.MissingPrivileges = append(d.MissingPrivileges, "INSERT")
	}
	if !upd {
		d.MissingPrivileges = append(d.MissingPrivileges, "UPDATE")
	}
	if !del {
		d.MissingPrivileges = append(d.MissingPrivileges, "DELETE")
	}
}
