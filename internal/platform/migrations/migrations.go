// Package migrations carries the SQL schema and the code that applies it.
//
// Two rules, and both are about blast radius rather than taste:
//
//  1. The .sql files are EMBEDDED, so the binary that runs migrations carries them. A
//     migration runner that reads a directory works on a laptop and fails in a container
//     that shipped without it -- discovered during a rollout, at the worst moment.
//
//  2. Migrations run ONLY from cmd/migrate, never on server boot. Running them at startup
//     means every replica in a rolling deploy races to alter the same schema; goose takes
//     an advisory lock so they serialise rather than corrupt, but they now serialise on a
//     DDL statement, so a slow ALTER stalls every pod's readiness at once. It also means a
//     rollback of the app implies a schema state nobody chose. In Kubernetes this is a Job
//     that runs to completion before the Deployment rolls.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
)

// FS holds the SQL files. Adding a file here is all it takes for goose to see it.
//
//go:embed *.sql
var FS embed.FS

// Up applies every pending migration.
func Up(ctx context.Context, db *sql.DB) error {
	p, err := provider(db)
	if err != nil {
		return err
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// DownTo rolls back to a specific version. Version 0 means "undo everything".
//
// Exported mainly so a test can prove up -> down-to-0 -> up produces an identical schema. A
// Down block nobody ever runs is a Down block that does not work, and you find that out in
// the middle of a failed release.
func DownTo(ctx context.Context, db *sql.DB, version int64) error {
	p, err := provider(db)
	if err != nil {
		return err
	}
	if _, err := p.DownTo(ctx, version); err != nil {
		return fmt.Errorf("roll back migrations: %w", err)
	}
	return nil
}

// Status returns the applied and pending migrations, for cmd/migrate to print.
func Status(ctx context.Context, db *sql.DB) ([]*goose.MigrationStatus, error) {
	p, err := provider(db)
	if err != nil {
		return nil, err
	}
	return p.Status(ctx)
}

// Version reports the current schema version.
func Version(ctx context.Context, db *sql.DB) (int64, error) {
	p, err := provider(db)
	if err != nil {
		return 0, err
	}
	return p.GetDBVersion(ctx)
}

// provider builds a goose provider over the embedded files.
//
// goose.NewProvider rather than the package-level goose.Up: the package-level API keeps
// global state (dialect, base filesystem), which makes two providers in one process --
// exactly what the test harness needs -- interfere with each other.
func provider(db *sql.DB) (*goose.Provider, error) {
	p, err := goose.NewProvider(goose.DialectPostgres, db, FS)
	if err != nil {
		return nil, fmt.Errorf("build migration provider: %w", err)
	}
	return p, nil
}
