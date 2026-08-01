package order

import (
	"context"
	"time"
)

// Store is the persistence port.
//
// Every method that reads or mutates tenant-scoped data takes tenantID as an explicit
// parameter. That is a deliberate ergonomic choice: it makes "which tenant is this
// query for?" impossible to leave unanswered at the call site. The GORM adapter ALSO
// enforces it structurally with a fail-closed callback, because a convention that is
// only enforced by review eventually is not enforced.
type Store interface {
	// Create persists a new order. It returns ErrDuplicate if the id already exists.
	Create(ctx context.Context, o Order) error

	// Get returns the order, or ErrNotFound. An order belonging to another tenant is
	// reported as ErrNotFound, never as PermissionDenied -- distinguishing the two
	// would leak the existence of other tenants' data.
	Get(ctx context.Context, tenantID, orderID string) (Order, error)

	// List returns a page of orders in a stable order. Implementations must use keyset
	// pagination, not OFFSET: offset pagination silently skips and repeats rows when
	// the underlying data changes between pages.
	List(ctx context.Context, tenantID string, f ListFilter) (Page, error)

	// Update persists changes to an existing order. It returns ErrNotFound if the order
	// does not exist within the tenant.
	Update(ctx context.Context, o Order) error
}

// Atomic runs work inside a single transaction.
//
// The callback receives the DOMAIN interfaces -- not the underlying *gorm.DB and not a
// driver-specific transaction handle. This matters more than it looks: handing the
// callback a driver type would force this package to import the database driver, invert
// the layering, and make the in-memory fake unable to implement Atomic. Every
// transactional business test would then silently require Docker.
//
// The publisher is passed alongside the store rather than injected once at construction
// because the outbox row MUST be written by the same transaction as the business change.
// That is the whole point of the outbox pattern: if the two writes could commit
// independently, you get orders with no event, or events for orders that rolled back.
// Passing it explicitly makes that coupling visible instead of hiding it in a context value.
type Atomic interface {
	InTx(ctx context.Context, fn func(Store, EventPublisher) error) error
}

// ListFilter selects and pages a subset of orders. Filters are typed fields rather than
// an expression language: a parsed filter DSL is a large attack and complexity surface,
// and it composes badly with statically analysable queries.
type ListFilter struct {
	// Status filters by exact status. StatusUnspecified means "any".
	Status Status

	// CustomerID filters by exact customer. Empty means "any".
	CustomerID string

	// PageSize is the caller's requested size. Zero means the server default.
	// Implementations clamp it to MaxPageSize.
	PageSize int32

	// PageToken is an opaque cursor from a previous Page.NextPageToken.
	PageToken string
}

// Page is one page of results.
type Page struct {
	Orders []Order

	// NextPageToken is empty when there are no further pages.
	NextPageToken string
}

// Page size bounds. The server always echoes the size it actually used, so a caller that
// asks for 10000 can tell it got 1000.
const (
	DefaultPageSize int32 = 50
	MaxPageSize     int32 = 1000
)

// ClampPageSize applies the default and the maximum.
func ClampPageSize(n int32) int32 {
	switch {
	case n <= 0:
		return DefaultPageSize
	case n > MaxPageSize:
		return MaxPageSize
	default:
		return n
	}
}

// EventPublisher is the outbound event port.
//
// The production implementation writes to the transactional outbox table using the same
// transaction as the business change, which is what makes "the order exists but the
// event was lost" impossible. Because it is an interface, the unit tier substitutes an
// in-memory recorder and asserts on what was published without a broker.
type EventPublisher interface {
	Publish(ctx context.Context, e Event) error
}

// Event is a domain event awaiting publication.
//
// It carries the domain snapshot, not encoded bytes: serialization is an adapter
// concern, and putting it here would drag protobuf into the domain package and break the
// import boundary that layout_test enforces.
type Event struct {
	// Type is the event name, e.g. EventOrderCreated.
	Type string

	// AggregateID is the order id. Adapters use it as the partition/ordering key so all
	// events for one order stay in sequence.
	AggregateID string

	TenantID string

	OccurredAt time.Time

	// Order is the aggregate snapshot at the time of the event.
	Order Order
}

// Event type names. These are part of the contract with consumers -- renaming one is a
// breaking change for every subscriber, exactly like renaming an RPC.
const (
	EventOrderCreated   = "order.v1.OrderCreated"
	EventOrderCancelled = "order.v1.OrderCancelled"
)
