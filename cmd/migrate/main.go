// Command migrate applies the database schema.
//
// A SEPARATE BINARY, and the separation is the point.
//
// Running migrations on server boot is the common shortcut and it fails in a specific,
// expensive way: every replica in a rolling deploy races to alter the same schema. goose
// takes an advisory lock so they serialise rather than corrupt, but now every pod's readiness
// waits behind one DDL statement -- so a slow ALTER on a large table stalls the entire
// rollout, and the symptom is "the deploy is stuck" rather than "the migration is slow". It
// also couples rollback of the application to rollback of the schema, which are different
// decisions with different risks.
//
// In Kubernetes this is a Job that runs to completion before the Deployment rolls. Locally it
// is one command. Either way the schema changes exactly once, under supervision.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver

	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/platform/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	timeout := flag.Duration("timeout", 5*time.Minute, "overall deadline for the migration")
	flag.Usage = usage
	flag.Parse()

	command := flag.Arg(0)
	if command == "" {
		usage()
		return fmt.Errorf("no command given")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	if cfg.Postgres.DSN.Reveal() == "" {
		return fmt.Errorf("POSTGRES_DSN is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// database/sql directly, not GORM. Migrations are DDL; an ORM adds nothing here and
	// would drag its model registration and callbacks into a binary whose entire job is to
	// execute SQL files.
	db, err := sql.Open("pgx", cfg.Postgres.DSN.Reveal())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}

	switch command {
	case "up":
		if err := migrations.Up(ctx, db); err != nil {
			return err
		}
		return printVersion(ctx, db)

	case "status":
		statuses, err := migrations.Status(ctx, db)
		if err != nil {
			return err
		}
		for _, s := range statuses {
			state := "pending"
			if !s.AppliedAt.IsZero() {
				state = s.AppliedAt.Format(time.RFC3339)
			}
			fmt.Printf("%-10s %s\n", state, s.Source.Path)
		}
		return nil

	case "version":
		return printVersion(ctx, db)

	case "down-to":
		// down-to takes an EXPLICIT target and there is no bare "down".
		//
		// A one-keystroke rollback of the most recent migration is how a production table
		// gets dropped by someone who meant to do it in staging. Naming the version forces
		// the operator to look at `migrate status` first and state where they intend to land.
		arg := flag.Arg(1)
		if arg == "" {
			return fmt.Errorf("down-to requires a version (see `migrate status`); use 0 to undo everything")
		}
		version, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			return fmt.Errorf("version %q is not a number", arg)
		}
		if err := migrations.DownTo(ctx, db, version); err != nil {
			return err
		}
		return printVersion(ctx, db)

	default:
		usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func printVersion(ctx context.Context, db *sql.DB) error {
	v, err := migrations.Version(ctx, db)
	if err != nil {
		return err
	}
	fmt.Printf("schema version: %d\n", v)
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `migrate applies the database schema.

Usage:
  migrate [flags] <command>

Commands:
  up               apply every pending migration
  status           list migrations and when they were applied
  version          print the current schema version
  down-to <n>      roll back to version n (0 undoes everything)

Configuration comes from the environment, same as the service:
  POSTGRES_DSN     required

Flags:
`)
	flag.PrintDefaults()
}
