// Package orderpg is the Postgres adapter for the order domain.
//
// It is the ONLY package besides internal/platform/gormx that may see a *gorm.DB. The domain
// declares its ports over domain types, so nothing above this package knows Postgres exists,
// and the in-memory store remains a complete substitute -- which is what keeps the default
// test tier free of Docker.
//
// The row structs below are deliberately separate from the domain aggregate rather than
// tagged onto it. Tagging order.Order with `gorm:"..."` would put persistence concerns in the
// domain, make the aggregate's field names a database migration, and break the import
// boundary test/layout_test.go enforces. The cost is the two mapping functions at the bottom
// of this file, which are dull, total, and exercised by the shared store contract.
package orderpg

import (
	"time"

	"github.com/example/gomicro/internal/order"
)

// orderRow is the `orders` table.
type orderRow struct {
	ID         string `gorm:"column:id;primaryKey"`
	TenantID   string `gorm:"column:tenant_id"`
	CustomerID string `gorm:"column:customer_id"`

	// Status stores the NAME. See the migration: storing the iota would couple every row to
	// the declaration order of the Go constants.
	Status string `gorm:"column:status"`

	CurrencyCode string `gorm:"column:currency_code"`
	TotalUnits   int64  `gorm:"column:total_units"`
	TotalNanos   int32  `gorm:"column:total_nanos"`

	// autoCreateTime/autoUpdateTime are explicitly OFF. The domain owns these timestamps --
	// order.Service sets them, the contract asserts they round-trip, and letting GORM
	// overwrite them on write would make CreatedAt mean "when the row was last saved".
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime:false"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime:false"`

	Items []itemRow `gorm:"foreignKey:OrderID;references:ID"`
}

func (orderRow) TableName() string { return "orders" }

// TenantColumn makes orderRow tenant-scoped, which is what arms the fail-closed guard in
// gormx for every Query, Update and Delete against this table.
func (orderRow) TenantColumn() string { return "tenant_id" }

// itemRow is the `order_items` table.
//
// It deliberately does NOT implement gormx.TenantScoped, because it has no tenant column.
// Its rows are reachable only through an order_id that came from a tenant-scoped query, so
// the scoping is transitive. A fork that starts querying order_items directly must either add
// a tenant_id column or accept that this table is not guarded -- worth knowing before writing
// that query, not after.
type itemRow struct {
	OrderID string `gorm:"column:order_id;primaryKey"`

	// LineNo preserves the caller's item order. Without an explicit ORDER BY on it, items
	// come back in whatever order the planner chooses, and the contract's round-trip
	// assertion fails intermittently -- the worst kind of failure to diagnose.
	LineNo int `gorm:"column:line_no;primaryKey"`

	SKU          string `gorm:"column:sku"`
	Quantity     int32  `gorm:"column:quantity"`
	UnitCurrency string `gorm:"column:unit_currency"`
	UnitUnits    int64  `gorm:"column:unit_units"`
	UnitNanos    int32  `gorm:"column:unit_nanos"`
}

func (itemRow) TableName() string { return "order_items" }

// outboxRow is the `outbox` table. The relay that drains it lands in M8a; this milestone
// only writes rows, inside the business transaction.
type outboxRow struct {
	ID          int64      `gorm:"column:id;primaryKey;autoIncrement"`
	TenantID    string     `gorm:"column:tenant_id"`
	AggregateID string     `gorm:"column:aggregate_id"`
	EventType   string     `gorm:"column:event_type"`
	Payload     []byte     `gorm:"column:payload;type:jsonb"`
	OccurredAt  time.Time  `gorm:"column:occurred_at"`
	PublishedAt *time.Time `gorm:"column:published_at"`
}

func (outboxRow) TableName() string { return "outbox" }

// toRow maps the aggregate onto its rows.
func toRow(o order.Order) (orderRow, []itemRow) {
	row := orderRow{
		ID:           o.ID,
		TenantID:     o.TenantID,
		CustomerID:   o.CustomerID,
		Status:       o.Status.String(),
		CurrencyCode: o.Total.CurrencyCode,
		TotalUnits:   o.Total.Units,
		TotalNanos:   o.Total.Nanos,
		// UTC, always. Postgres timestamptz stores an instant, but the Go value carries a
		// location, and a round trip through the driver returns it in the session's zone --
		// so comparing a stored time against a locally-constructed one differs by offset
		// unless both are normalised. Normalising on the way in is the half we control.
		CreatedAt: o.CreatedAt.UTC(),
		UpdatedAt: o.UpdatedAt.UTC(),
	}

	items := make([]itemRow, 0, len(o.Items))
	for i, item := range o.Items {
		items = append(items, itemRow{
			OrderID:      o.ID,
			LineNo:       i,
			SKU:          item.SKU,
			Quantity:     item.Quantity,
			UnitCurrency: item.UnitPrice.CurrencyCode,
			UnitUnits:    item.UnitPrice.Units,
			UnitNanos:    item.UnitPrice.Nanos,
		})
	}
	return row, items
}

// toDomain maps rows back onto the aggregate.
func toDomain(row orderRow) (order.Order, error) {
	status, err := order.ParseStatus(row.Status)
	if err != nil {
		// A status string the domain does not recognise means the database holds a value
		// this binary cannot interpret -- a failed rollback, or a newer version having
		// written rows an older one is now reading. Returning the error beats guessing.
		return order.Order{}, err
	}

	total, err := order.NewMoney(row.CurrencyCode, row.TotalUnits, row.TotalNanos)
	if err != nil {
		return order.Order{}, err
	}

	items := make([]order.Item, 0, len(row.Items))
	for _, it := range row.Items {
		price, err := order.NewMoney(it.UnitCurrency, it.UnitUnits, it.UnitNanos)
		if err != nil {
			return order.Order{}, err
		}
		items = append(items, order.Item{
			SKU:       it.SKU,
			Quantity:  it.Quantity,
			UnitPrice: price,
		})
	}

	return order.Order{
		ID:         row.ID,
		TenantID:   row.TenantID,
		CustomerID: row.CustomerID,
		Status:     status,
		Items:      items,
		Total:      total,
		CreatedAt:  row.CreatedAt.UTC(),
		UpdatedAt:  row.UpdatedAt.UTC(),
	}, nil
}
