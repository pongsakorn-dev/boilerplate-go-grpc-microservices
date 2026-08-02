-- +goose Up

-- THE OUTBOX QUARANTINE.
--
-- M8a's relay abandoned the whole batch on any publish error and reclaimed the same rows on
-- the next drain. That is correct when every failure is transient -- a broker restart heals
-- itself. It is a permanent outage when a failure is NOT transient, and M8b made that
-- reachable: a payload above the NATS server's max_payload (1 MiB by default) is rejected
-- identically on every attempt, so one oversized event stops the outbox for every event
-- behind it, forever, with nothing in the logs but the same error repeating.
--
-- A quarantined row is set aside rather than deleted: the claim query skips it, the relay
-- keeps moving, and the row stays in the table with the reason attached so an operator can
-- fix the cause and replay it with
--
--     UPDATE outbox SET failed_at = NULL, failure_reason = NULL WHERE id = ...;
--
-- Nothing is ever discarded, which is the property the outbox exists to provide.
ALTER TABLE outbox ADD COLUMN failed_at      timestamptz;
ALTER TABLE outbox ADD COLUMN failure_reason text;

-- The partial index has to learn about quarantine too, or the relay's claim scans the growing
-- pile of quarantined rows on every drain forever.
DROP INDEX outbox_unpublished_idx;
CREATE INDEX outbox_unpublished_idx ON outbox (id) WHERE published_at IS NULL AND failed_at IS NULL;

-- CONSUMER-SIDE DEDUPLICATION.
--
-- JetStream's Nats-Msg-Id window collapses the relay's republishing, but it is a WINDOW: a
-- republish arriving after it is an ordinary new message, and redelivery after a handler
-- crash is not deduplicated at all. At-least-once delivery is the contract, so the consumer
-- is where effectively-once is actually built.
--
-- The primary key is what does the work. The handler inserts here and applies its effect in
-- ONE transaction, so either both happen or neither does -- there is no window in which the
-- effect has landed but the record of it has not.
CREATE TABLE processed_events (
    consumer     text        NOT NULL,
    message_id   text        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),

    -- Keyed by CONSUMER as well as message id. Two consumers of the same stream must each
    -- process every message; keying on the message alone would let whichever ran first
    -- silently suppress the event for the other.
    PRIMARY KEY (consumer, message_id)
);

-- Supports pruning, which this template does not ship.
--
-- Said plainly rather than left implicit: processed_events grows forever. It only needs to
-- outlive the stream's retention (NATS_STREAM_MAX_AGE), because a message the broker has
-- dropped can never be redelivered, so DELETE FROM processed_events WHERE processed_at <
-- now() - interval '8 days' is a safe periodic job for the shipped 7-day default.
CREATE INDEX processed_events_processed_at_idx ON processed_events (processed_at);

-- A READ MODEL, and the reason the consumer in this template does something real.
--
-- A projection is the canonical outbox consumer, and it is the case where duplicate delivery
-- is VISIBLE rather than theoretical: applying order.created twice increments the count
-- twice, and the wrong number is still there tomorrow. That makes the processed_events
-- transaction demonstrable instead of merely described -- orderproj's test deletes the dedup
-- row and watches the count double.
CREATE TABLE order_counts (
    tenant_id    text        PRIMARY KEY,
    orders       bigint      NOT NULL DEFAULT 0,
    last_order_at timestamptz,

    CONSTRAINT order_counts_tenant_not_empty CHECK (tenant_id <> ''),
    CONSTRAINT order_counts_orders_not_negative CHECK (orders >= 0)
);

-- +goose Down

DROP TABLE order_counts;
DROP TABLE processed_events;

DROP INDEX outbox_unpublished_idx;
CREATE INDEX outbox_unpublished_idx ON outbox (id) WHERE published_at IS NULL;

ALTER TABLE outbox DROP COLUMN failure_reason;
ALTER TABLE outbox DROP COLUMN failed_at;
