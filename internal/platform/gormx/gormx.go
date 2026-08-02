// Package gormx holds everything GORM-specific that is not an order.
//
// GORM was chosen over a query builder for reach: it is the ORM most Go teams already know,
// and CRUD is fast to write in it. What it gives up is compile-time column checking, so
// three mechanisms in this package are load-bearing rather than optional:
//
//   - tenant.go   a fail-closed callback, because there are no .sql files to lint
//   - counter.go  a query counter, because N+1 is GORM's characteristic failure and is
//     invisible until production
//   - drift, in orderpg's integration test: AutoMigrate in DryRun against the goose schema
//     must want to change nothing, which is the closest thing to the compile error GORM
//     gave up
//
// *gorm.DB never escapes this package and internal/order/orderpg. The domain's Atomic port
// is declared over the DOMAIN Store, so the in-memory fake can implement it and every
// transactional business test stays Docker-free.
package gormx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/example/gomicro/internal/platform/config"
)

// Open connects, configures the pool explicitly, and installs the guards.
func Open(ctx context.Context, cfg config.Config, log *slog.Logger) (*gorm.DB, error) {
	dsn := cfg.Postgres.DSN.Reveal()
	if dsn == "" {
		return nil, errors.New("POSTGRES_DSN is empty")
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: NewSlogLogger(log, slowQueryThreshold),

		// GORM's default transaction wraps every single Create/Update in its own
		// transaction, which doubles the round trips for writes that do not need it. The
		// writes that DO need atomicity go through the Atomic port and say so explicitly.
		SkipDefaultTransaction: true,

		// Without this, every write to a model with associations issues extra statements to
		// upsert them. orderpg writes items explicitly, in one batch, so the implicit
		// behaviour is pure cost -- and it is the kind of cost that shows up as N+1.
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("unwrap sql.DB: %w", err)
	}
	configurePool(sqlDB, cfg)

	if err := RegisterTenantGuard(db); err != nil {
		return nil, fmt.Errorf("register tenant guard: %w", err)
	}

	// Ping with the caller's context so a hung database fails startup on a deadline rather
	// than hanging the process before it can serve its health endpoint.
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	return db, nil
}

// slowQueryThreshold is when a query gets logged as slow. Chosen to be well above a healthy
// indexed lookup and well below anything a user would tolerate.
const slowQueryThreshold = 200 * time.Millisecond

// configurePool sets every pool bound EXPLICITLY.
//
// database/sql's defaults are actively wrong in a container. MaxIdleConns defaults to 2 --
// so a service under load closes and reopens connections constantly, paying a TLS handshake
// and a Postgres backend fork each time -- and MaxOpenConns defaults to UNLIMITED, so twenty
// replicas will happily open more connections than a stock max_connections of 100 allows.
//
// The failure that produces looks like a database outage ("too many clients already") rather
// than a configuration mistake, and it arrives at exactly the moment traffic grows. Setting
// all four here, from values config.Validate insists are explicit, is the cheapest way to
// never have that conversation.
func configurePool(sqlDB *sql.DB, cfg config.Config) {
	sqlDB.SetMaxOpenConns(cfg.Postgres.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.Postgres.MaxIdleConns)

	// ConnMaxLifetime also serves a second purpose: it is what lets a failover or a rolling
	// database upgrade actually take effect, because connections pinned to the old primary
	// are eventually retired instead of living forever.
	sqlDB.SetConnMaxLifetime(cfg.Postgres.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(cfg.Postgres.ConnMaxIdleTime)
}

// NewSlogLogger adapts GORM's logger onto log/slog.
//
// Without it GORM writes its own format to stdout, so half the service's output is
// structured JSON and half is not -- and the half that is not carries no trace_id, which
// means the slow query you are chasing cannot be correlated with the request that caused it.
func NewSlogLogger(log *slog.Logger, slow time.Duration) gormlogger.Interface {
	return &slogLogger{log: log, slow: slow}
}

type slogLogger struct {
	log  *slog.Logger
	slow time.Duration
}

func (l *slogLogger) LogMode(gormlogger.LogLevel) gormlogger.Interface { return l }

func (l *slogLogger) Info(ctx context.Context, msg string, args ...any) {
	l.log.InfoContext(ctx, fmt.Sprintf(msg, args...))
}

func (l *slogLogger) Warn(ctx context.Context, msg string, args ...any) {
	l.log.WarnContext(ctx, fmt.Sprintf(msg, args...))
}

func (l *slogLogger) Error(ctx context.Context, msg string, args ...any) {
	l.log.ErrorContext(ctx, fmt.Sprintf(msg, args...))
}

// Trace is called once per statement.
//
// The SQL is logged at DEBUG and the slow-query warning carries it too. That is a deliberate
// trade: query text can contain parameter values, and parameter values can be personal data,
// so a service handling sensitive rows should log the query WITHOUT its arguments or drop
// the text entirely. Saying so here is more useful than pretending the default is safe
// everywhere.
func (l *slogLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	elapsed := time.Since(begin)

	switch {
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound):
		// ErrRecordNotFound is excluded on purpose: "no such order" is an ordinary outcome
		// of a lookup, and logging it at ERROR makes the error rate meaningless.
		sqlText, rows := fc()
		l.log.ErrorContext(ctx, "sql error",
			slog.String("error", err.Error()),
			slog.String("sql", sqlText),
			slog.Int64("rows", rows),
			slog.Duration("elapsed", elapsed))

	case l.slow > 0 && elapsed >= l.slow:
		sqlText, rows := fc()
		l.log.WarnContext(ctx, "slow sql",
			slog.String("sql", sqlText),
			slog.Int64("rows", rows),
			slog.Duration("elapsed", elapsed),
			slog.Duration("threshold", l.slow))

	default:
		// Cheap guard: fc() formats the SQL, so calling it unconditionally would build a
		// string on every query only to discard it when debug logging is off.
		if l.log.Enabled(ctx, slog.LevelDebug) {
			sqlText, rows := fc()
			l.log.DebugContext(ctx, "sql",
				slog.String("sql", sqlText),
				slog.Int64("rows", rows),
				slog.Duration("elapsed", elapsed))
		}
	}
}
