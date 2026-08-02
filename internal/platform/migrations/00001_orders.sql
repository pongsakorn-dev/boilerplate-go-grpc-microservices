-- +goose Up

-- MONEY IS STORED AS AN EXACT INTEGER PAIR, NOT numeric(19,4).
--
-- The domain Money type is google.type.Money's shape: a currency code, whole units, and
-- nanos (10^-9). numeric(19,4) holds four decimal places, so it cannot represent nanos --
-- writing one through it truncates, silently, which is the exact class of money bug the
-- integral domain type exists to prevent. Two integer columns round-trip it exactly and
-- need no conversion at all.
--
-- A fork that wants SQL-side aggregation should add a generated numeric(19,9) column rather
-- than change these: keep the exact representation authoritative, and derive the convenient
-- one from it.
CREATE TABLE orders (
    id            uuid        PRIMARY KEY,
    tenant_id     text        NOT NULL,
    customer_id   text        NOT NULL,

    -- The NAME, not the iota. Storing the numeric value would couple every row to the
    -- declaration order of the Go constants; inserting a status in the middle would
    -- reinterpret existing rows. It also makes the table readable during an incident.
    status        text        NOT NULL,

    currency_code char(3)     NOT NULL,
    total_units   bigint      NOT NULL,
    total_nanos   integer     NOT NULL,

    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,

    CONSTRAINT orders_tenant_not_empty CHECK (tenant_id <> ''),
    CONSTRAINT orders_nanos_range CHECK (total_nanos > -1000000000 AND total_nanos < 1000000000)
);

-- THE KEYSET INDEX. Column order matches the query exactly: equality on tenant_id first,
-- then the (created_at, id) sort key.
--
-- Without it, every List degrades to a sequential scan plus an in-memory sort, and keyset
-- pagination's whole advantage over OFFSET disappears -- you would be walking the table on
-- every page anyway. orderpg_integration_test.go asserts EXPLAIN uses an index scan, because
-- "there is an index" and "the planner uses it" are different claims.
CREATE INDEX orders_keyset_idx ON orders (tenant_id, created_at, id);

CREATE TABLE order_items (
    order_id      uuid    NOT NULL REFERENCES orders (id) ON DELETE CASCADE,

    -- line_no preserves the caller's item order. Without it the items come back in whatever
    -- order the planner returns them, and an order whose lines silently reorder between
    -- writes and reads fails the contract's round-trip assertion.
    line_no       integer NOT NULL,

    sku           text    NOT NULL,
    quantity      integer NOT NULL,
    unit_currency char(3) NOT NULL,
    unit_units    bigint  NOT NULL,
    unit_nanos    integer NOT NULL,

    PRIMARY KEY (order_id, line_no),
    CONSTRAINT order_items_quantity_positive CHECK (quantity > 0)
);

-- THE OUTBOX, shipped here rather than in M8 because the store contract needs it.
--
-- The contract asserts that a rolled-back transaction writes neither the order nor its
-- event, and that a committed one writes both. That assertion is the reason the outbox
-- pattern exists, and it cannot run against Postgres unless this table does. The RELAY that
-- drains it -- FOR UPDATE SKIP LOCKED, batching, publishing -- is M8a; only the table and
-- the transactional insert are here.
CREATE TABLE outbox (
    id           bigserial   PRIMARY KEY,
    tenant_id    text        NOT NULL,
    aggregate_id uuid        NOT NULL,
    event_type   text        NOT NULL,
    payload      jsonb       NOT NULL,
    occurred_at  timestamptz NOT NULL,

    -- NULL until the relay has published it. A partial index on the unpublished rows keeps
    -- the relay's claim query cheap as the table grows, which matters because the table
    -- grows forever until something prunes it.
    published_at timestamptz
);

CREATE INDEX outbox_unpublished_idx ON outbox (id) WHERE published_at IS NULL;

-- +goose Down

DROP TABLE outbox;
DROP TABLE order_items;
DROP TABLE orders;
