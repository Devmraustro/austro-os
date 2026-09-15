package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// canaryWorkspaceA and canaryWorkspaceB are the two synthetic workspaces the
// boot-time isolation canary uses. They are fixed so a probe row left behind by
// an interrupted run is recognisable and cleaned up by the next one, and they
// sit in an obviously non-routable UUID range so they cannot collide with a
// real tenant.
const (
	canaryWorkspaceA = "deadbeef-0000-4000-8000-00000000000a"
	canaryWorkspaceB = "deadbeef-0000-4000-8000-00000000000b"
	canaryName       = "austro-rls-canary"
)

// VerifyRuntimeSecurity proves, against the live connection the application is
// about to serve traffic on, that the workspace isolation boundary is real
// rather than merely declared.
//
// Every check here observes the database rather than this process's own source.
// The alternative — trusting that the bootstrap enabled RLS, created the
// policies and created an unprivileged role — is exactly what allowed an
// owner-privileged runtime to pass a fully green test suite: the policies were
// present, correct, and not applied to the role that mattered.
//
// It is fatal on any mismatch. A process that cannot prove its own isolation
// boundary must not accept requests.
func VerifyRuntimeSecurity(ctx context.Context, db *sql.DB, expectedRole string) error {
	if err := verifyRoleAttributes(ctx, db, expectedRole); err != nil {
		return err
	}
	if err := verifyTableOwnership(ctx, db); err != nil {
		return err
	}
	if err := verifyRLSForced(ctx, db); err != nil {
		return err
	}
	return verifyIsolationLive(ctx, db)
}

// verifyRoleAttributes asserts the connected principal is not a superuser and
// does not hold BYPASSRLS. Either attribute makes PostgreSQL skip row level
// security entirely — including under FORCE — which would render every policy
// in the schema decorative while leaving the process looking correctly
// configured.
func verifyRoleAttributes(ctx context.Context, db *sql.DB, expectedRole string) error {
	var current string
	var super, bypass bool
	err := db.QueryRowContext(ctx, `
		SELECT r.rolname, r.rolsuper, r.rolbypassrls
		FROM pg_roles r WHERE r.rolname = current_user`).Scan(&current, &super, &bypass)
	if err != nil {
		return fmt.Errorf("runtime role attributes: %w", err)
	}
	if expectedRole != "" && !strings.EqualFold(current, expectedRole) {
		return fmt.Errorf(
			"connected as %q but the configured application runtime role is %q; "+
				"application traffic must not use the schema owner credential",
			current, expectedRole)
	}
	if super {
		return fmt.Errorf("application runtime role %q is a superuser, which bypasses row level security", current)
	}
	if bypass {
		return fmt.Errorf("application runtime role %q holds BYPASSRLS, which bypasses row level security", current)
	}
	return nil
}

// verifyTableOwnership asserts the runtime role owns none of the protected
// tables. PostgreSQL does not apply row level security to a table's owner, so
// ownership by the runtime role would be a silent, total isolation bypass even
// with the policies correct.
func verifyTableOwnership(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_roles r ON r.oid = c.relowner
		WHERE n.nspname = 'public'
		  AND c.relkind = 'r'
		  AND c.relname = ANY($1)
		  AND r.rolname = current_user`, rlsTables())
	if err != nil {
		return fmt.Errorf("table ownership check: %w", err)
	}
	defer rows.Close()
	var owned []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("table ownership check: %w", err)
		}
		owned = append(owned, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("table ownership check: %w", err)
	}
	if len(owned) > 0 {
		return fmt.Errorf(
			"application runtime role owns protected table(s) %s; row level security is not applied to a table's owner",
			strings.Join(owned, ", "))
	}
	return nil
}

// verifyRLSForced asserts every protected table has row level security both
// enabled and forced. Enabled-but-not-forced is the state that let an owner
// connection through; forced-but-not-enabled does not exist in PostgreSQL, but
// checking both flags means a partially applied migration cannot pass.
func verifyRLSForced(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
		SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
		  AND c.relkind = 'r'
		  AND c.relname = ANY($1)`, rlsTables())
	if err != nil {
		return fmt.Errorf("rls flag check: %w", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	var notEnabled, notForced, missing []string
	for rows.Next() {
		var name string
		var enabled, forced bool
		if err := rows.Scan(&name, &enabled, &forced); err != nil {
			return fmt.Errorf("rls flag check: %w", err)
		}
		seen[name] = true
		if !enabled {
			notEnabled = append(notEnabled, name)
		}
		if !forced {
			notForced = append(notForced, name)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rls flag check: %w", err)
	}
	for _, t := range rlsTables() {
		if !seen[t] {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("protected table(s) absent from the schema: %s", strings.Join(missing, ", "))
	}
	if len(notEnabled) > 0 {
		return fmt.Errorf("row level security not enabled on: %s", strings.Join(notEnabled, ", "))
	}
	if len(notForced) > 0 {
		return fmt.Errorf(
			"row level security not FORCED on: %s; without FORCE the table owner is unconstrained",
			strings.Join(notForced, ", "))
	}
	return nil
}

// verifyIsolationLive runs an end-to-end isolation canary through the very
// connection the application will use: commit a row as workspace A, then prove
// from a transaction bound to workspace B that A's row is invisible while B's
// own row is visible.
//
// A committed row is required. Doing this inside one rolled-back transaction
// would prove nothing, because the second read would be filtered by the
// rollback rather than by the policy. The canary asserts both directions: a
// policy that hides nothing fails on the cross-workspace count, and a policy
// that hides everything fails on the own-workspace count.
func verifyIsolationLive(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	a, err := uuid.Parse(canaryWorkspaceA)
	if err != nil {
		return fmt.Errorf("canary workspace a: %w", err)
	}
	b, err := uuid.Parse(canaryWorkspaceB)
	if err != nil {
		return fmt.Errorf("canary workspace b: %w", err)
	}

	// Seed both rows, each from a transaction bound to its own workspace so the
	// WITH CHECK expression (which defaults to the USING expression) accepts it.
	for _, ws := range []uuid.UUID{a, b} {
		if err := inWorkspace(ctx, db, ws, func(tx *sql.Tx) error {
			// Remove a probe left behind by an interrupted run before re-seeding,
			// otherwise the primary key insert fails.
			if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, ws); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO workspaces (id, name) VALUES ($1, $2)`, ws, canaryName)
			return err
		}); err != nil {
			return fmt.Errorf("isolation canary seed: %w", err)
		}
	}

	// Read from workspace B's point of view.
	var own, other int
	err = inWorkspace(ctx, db, b, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM workspaces WHERE id = $1`, b).Scan(&own); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM workspaces WHERE id = $1`, a).Scan(&other)
	})
	if err != nil {
		return fmt.Errorf("isolation canary read: %w", err)
	}

	// Always attempt cleanup, even when the assertion below fails, so a failed
	// boot does not leave tenant-shaped debris behind.
	for _, ws := range []uuid.UUID{a, b} {
		_ = inWorkspace(ctx, db, ws, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, ws)
			return err
		})
	}

	if other != 0 {
		return fmt.Errorf(
			"workspace isolation FAILED at runtime: a session bound to workspace %s saw %d row(s) belonging to workspace %s",
			b, other, a)
	}
	if own != 1 {
		return fmt.Errorf(
			"workspace isolation policy is over-restrictive: a session bound to workspace %s could not see its own row (got %d, want 1)",
			b, own)
	}
	return nil
}

// inWorkspace runs fn inside a transaction with app.current_workspace bound
// transaction-locally, which is the same discipline every store uses, so the
// canary exercises the production access pattern rather than a special one.
func inWorkspace(ctx context.Context, db *sql.DB, ws uuid.UUID, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_workspace', $1, true)`, ws.String()); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
