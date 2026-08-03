package orderpg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"go.opentelemetry.io/otel/propagation"

	"github.com/example/gomicro/internal/order"
	"github.com/example/gomicro/internal/platform/gormx"
)

// Store is the Postgres implementation of order.Store, order.Atomic and
// order.EventPublisher.
//
// One type implements all three because the transactional variant must offer exactly the
// same behaviour bound to a transaction: InTx hands the callback a *Store whose db is the
// tx. Two types would be two code paths, and the one used only inside transactions is the
// one nobody exercises.
type Store struct {
	db *gorm.DB
}

// New wraps an already-configured *gorm.DB. Configuration -- pool, logger, tenant guard --
// belongs to gormx.Open, so this constructor cannot be handed a database that skipped it.
func New(db *gorm.DB) *Store { return &Store{db: db} }

// PostgreSQL error codes. Class 23 is integrity constraint violation.
const (
	pgUniqueViolation = "23505"
	pgCheckViolation  = "23514"
)

// Create inserts the order and its items.
func (s *Store) Create(ctx context.Context, o order.Order) error {
	if o.TenantID == "" {
		// Checked here as well as by the CHECK constraint. The constraint is the guarantee;
		// this is the readable error, and it costs one comparison rather than a round trip.
		return order.ErrMissingTenant
	}

	row, items := toRow(o)
	db := s.scoped(ctx, o.TenantID)

	return s.inTransaction(db, func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return mapWriteError(err)
		}
		if len(items) > 0 {
			// One multi-row INSERT, not one per item. The N+1 guard asserts the count.
			if err := tx.Create(&items).Error; err != nil {
				return mapWriteError(err)
			}
		}
		return nil
	})
}

// Get returns one order within the tenant.
func (s *Store) Get(ctx context.Context, tenantID, orderID string) (order.Order, error) {
	if tenantID == "" {
		return order.Order{}, order.ErrMissingTenant
	}

	var row orderRow
	err := s.scoped(ctx, tenantID).
		Preload("Items", func(db *gorm.DB) *gorm.DB { return db.Order("line_no") }).
		Where("tenant_id = ? AND id = ?", tenantID, orderID).
		First(&row).Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		// Never distinguished from "belongs to another tenant". The tenant predicate is in
		// the WHERE clause, so a hit in another tenant simply does not match -- the store
		// cannot tell the difference either, which is the strongest form of this guarantee.
		return order.Order{}, order.ErrNotFound
	}
	if err != nil {
		return order.Order{}, err
	}
	return toDomain(row)
}

// List returns a page using KEYSET pagination.
func (s *Store) List(ctx context.Context, tenantID string, f order.ListFilter) (order.Page, error) {
	if tenantID == "" {
		return order.Page{}, order.ErrMissingTenant
	}

	after, afterID, hasCursor, err := order.DecodePageToken(tenantID, f)
	if err != nil {
		return order.Page{}, err
	}

	// THE TENANT PREDICATE IS WRITTEN HERE EXPLICITLY, not left to the callback.
	//
	// gormx's guard would inject it too, and for a while this relied on that alone. Deleting
	// the guard's registration and re-running the suite showed what that costs: List returned
	// BOTH tenants' rows and only the guard's own unit test noticed. A single mechanism whose
	// removal silently produces cross-tenant reads is not defence in depth, it is a single
	// point of failure with a comment claiming otherwise.
	//
	// So both: this clause makes the query correct on its own and visible at the call site,
	// and the callback catches the query somebody forgets to write it on.
	q := s.scoped(ctx, tenantID).
		Model(&orderRow{}).
		Preload("Items", func(db *gorm.DB) *gorm.DB { return db.Order("line_no") }).
		Where("tenant_id = ?", tenantID)

	if f.Status != order.StatusUnspecified {
		q = q.Where("status = ?", f.Status.String())
	}
	if f.CustomerID != "" {
		q = q.Where("customer_id = ?", f.CustomerID)
	}
	if hasCursor {
		// A ROW-VALUE comparison, not "created_at > ? OR (created_at = ? AND id > ?)".
		//
		// The two are logically identical and the planner treats them very differently.
		// Measured on Postgres 17 against the shipped index, with EXPLAIN:
		//
		//	row-value  Index Cond: tenant_id = ... AND ROW(created_at, id) > ROW(...)
		//	OR form    Index Cond: tenant_id = ...
		//	           Filter:     created_at > ... OR (created_at = ... AND id > ...)
		//
		// Both "use the index". Only the row-value form puts the SORT KEY in the index
		// condition, so it seeks directly to the resume point; the OR form seeks on tenant
		// alone and then filters every one of that tenant's rows in memory -- which is
		// precisely the linear scan keyset pagination exists to avoid, reintroduced by a
		// rewrite that looks equivalent.
		//
		// An earlier version of this comment claimed the OR form "does not match the index",
		// which is false, and the integration test's assertion was correspondingly weak
		// enough to pass for both. It now asserts the predicate is in the Index Cond rather
		// than merely that the index is named.
		q = q.Where("(created_at, id) > (?, ?)", after.UTC(), afterID)
	}

	size := int(order.ClampPageSize(f.PageSize))

	// Fetch one MORE than the page size. That extra row is how "is there a next page?" is
	// answered without a second COUNT query -- and a COUNT over a filtered set is exactly
	// the query that gets slow first.
	var rows []orderRow
	if err := q.Order("created_at, id").Limit(size + 1).Find(&rows).Error; err != nil {
		return order.Page{}, err
	}

	page := order.Page{}
	hasMore := len(rows) > size
	if hasMore {
		rows = rows[:size]
	}

	page.Orders = make([]order.Order, 0, len(rows))
	for _, row := range rows {
		o, err := toDomain(row)
		if err != nil {
			return order.Page{}, err
		}
		page.Orders = append(page.Orders, o)
	}

	if hasMore && len(page.Orders) > 0 {
		token, err := order.EncodePageToken(tenantID, f, page.Orders[len(page.Orders)-1])
		if err != nil {
			return order.Page{}, err
		}
		page.NextPageToken = token
	}
	return page, nil
}

// Update replaces the order and its items.
func (s *Store) Update(ctx context.Context, o order.Order) error {
	if o.TenantID == "" {
		return order.ErrMissingTenant
	}

	row, items := toRow(o)
	db := s.scoped(ctx, o.TenantID)

	return s.inTransaction(db, func(tx *gorm.DB) error {
		// Select the columns explicitly so a zero-valued field still writes. GORM's Updates
		// with a struct skips zero values, which would silently refuse to clear a field --
		// the sort of behaviour that looks like a caching bug for a week.
		res := tx.Model(&orderRow{}).
			Where("tenant_id = ? AND id = ?", o.TenantID, o.ID).
			Select("customer_id", "status", "currency_code", "total_units", "total_nanos", "updated_at").
			Updates(map[string]any{
				"customer_id":   row.CustomerID,
				"status":        row.Status,
				"currency_code": row.CurrencyCode,
				"total_units":   row.TotalUnits,
				"total_nanos":   row.TotalNanos,
				"updated_at":    row.UpdatedAt,
			})
		if res.Error != nil {
			return mapWriteError(res.Error)
		}
		if res.RowsAffected == 0 {
			// Zero rows means either no such order or another tenant's -- and the tenant
			// predicate is already in the WHERE clause, so both arrive here identically.
			return order.ErrNotFound
		}

		// Replace the items wholesale. Diffing them would be more efficient and much easier
		// to get subtly wrong; orders have a handful of lines, so correctness wins.
		// order_items carries no tenant column, so it is scoped transitively: this runs only
		// after the UPDATE above matched a row within the caller's tenant.
		if err := tx.Where("order_id = ?", o.ID).Delete(&itemRow{}).Error; err != nil {
			return err
		}
		if len(items) > 0 {
			if err := tx.Create(&items).Error; err != nil {
				return mapWriteError(err)
			}
		}
		return nil
	})
}

// InTx runs fn inside a single transaction.
//
// The callback receives the DOMAIN interfaces, never *gorm.DB. That is what lets the
// in-memory fake implement order.Atomic too, so transactional business tests -- including
// "a rolled back transaction writes neither the order nor the event" -- run without Docker.
func (s *Store) InTx(ctx context.Context, fn func(order.Store, order.EventPublisher) error) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		inner := &Store{db: tx}
		return fn(inner, inner)
	})
}

// Publish writes an event to the outbox.
//
// It takes no broker and does no network I/O, and that is the whole design: the event is
// inserted by the SAME transaction as the business change, so "the order committed but the
// event was lost" is not a state the database can hold. The relay that drains this table and
// publishes to a broker is M8a.
func (s *Store) Publish(ctx context.Context, e order.Event) error {
	payload, err := json.Marshal(e.Order)
	if err != nil {
		return fmt.Errorf("encode event payload: %w", err)
	}

	row := outboxRow{
		TenantID:    e.TenantID,
		AggregateID: e.AggregateID,
		EventType:   e.Type,
		Payload:     payload,
		OccurredAt:  e.OccurredAt.UTC(),

		// CAPTURED IN THE ADAPTER, deliberately, and not in the domain.
		//
		// order.Event has no trace field and must not grow one: internal/order imports no
		// telemetry SDK and test/layout_test.go enforces that, so the domain could not read a
		// span even if it wanted to. This adapter already sits outside that boundary -- it
		// imports GORM -- which makes it the first place on the write path allowed to look.
		//
		// It is also the LAST place that can. The request's context ends when the handler
		// returns; the relay picks this row up later, from another process, with nothing left
		// to recover the trace from.
		TraceParent: traceParentOf(ctx),
	}

	// Session with NewDB false keeps the transaction, if any; the outbox table is not
	// tenant-scoped in gormx's sense, so no tenant context is required here.
	return s.db.WithContext(ctx).Create(&row).Error
}

// traceParentOf renders the active span as a W3C traceparent, or "" when there is none.
//
// propagation.TraceContext directly rather than otel.GetTextMapPropagator(), for two reasons.
// The global is a composite that also carries Baggage, and baggage is caller-supplied data with
// no size bound -- storing it in a database column per event is an unbounded write amplifier
// nobody asked for. And the global is set up by observability.NewTracerProvider, so depending
// on it here would make this function's behaviour depend on process startup order, which is
// exactly the kind of thing that works in production and returns "" in a test.
func traceParentOf(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// Events reads everything in the outbox, oldest first.
//
// Exported so the shared store contract can assert on published events against Postgres
// exactly as it does against the in-memory recorder. Without it the two transactional
// subtests -- the ones that justify the outbox existing at all -- could only run against the
// fake, which is where they prove the least.
func (s *Store) Events(ctx context.Context) ([]order.Event, error) {
	var rows []outboxRow
	if err := s.db.WithContext(ctx).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}

	events := make([]order.Event, 0, len(rows))
	for _, row := range rows {
		var o order.Order
		if err := json.Unmarshal(row.Payload, &o); err != nil {
			return nil, fmt.Errorf("decode event payload: %w", err)
		}
		events = append(events, order.Event{
			Type:        row.EventType,
			AggregateID: row.AggregateID,
			TenantID:    row.TenantID,
			OccurredAt:  row.OccurredAt.UTC(),
			Order:       o,
		})
	}
	return events, nil
}

// scoped attaches the tenant to the context so the fail-closed guard in gormx can inject its
// predicate, and returns a session bound to it.
func (s *Store) scoped(ctx context.Context, tenantID string) *gorm.DB {
	return s.db.WithContext(gormx.WithTenant(ctx, tenantID))
}

// inTransaction runs fn in a transaction, reusing an existing one when already inside it.
//
// GORM nests via SAVEPOINT, so calling this from within InTx does not open a second
// connection-level transaction and does not deadlock against the outer one.
func (s *Store) inTransaction(db *gorm.DB, fn func(*gorm.DB) error) error {
	return db.Transaction(fn)
}

// mapWriteError translates driver errors into domain sentinels.
//
// The SQLSTATE, not the message text. Message text is localised, changes between Postgres
// releases, and differs between pgx and lib/pq -- matching on it is a string comparison that
// works until an upgrade. 23505 is 23505 forever.
func mapWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation:
			return order.ErrDuplicate
		case pgCheckViolation:
			// The CHECK constraints in the migration are the last line of defence for
			// invariants the domain already enforces (non-empty tenant, positive quantity,
			// nanos in range). Reaching one means something bypassed validation.
			return fmt.Errorf("%w: constraint %s", order.ErrInvalidItem, pgErr.ConstraintName)
		}
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return order.ErrDuplicate
	}
	return err
}

// Compile-time proof that the adapter satisfies every port. Without these, a signature drift
// surfaces only wherever the store happens to be wired.
var (
	_ order.Store          = (*Store)(nil)
	_ order.Atomic         = (*Store)(nil)
	_ order.EventPublisher = (*Store)(nil)
)
