-- +goose Up

-- THE TRACE HAS TO BE STORED, because by the time the relay runs there is nothing to read it
-- from.
--
-- Every other hop in this system propagates trace context in-band: gRPC metadata, an HTTP
-- header, something the caller is holding while the callee runs. The outbox is the one place
-- where the producer and the consumer are separated by TIME rather than by a network. The
-- request that wrote this row returned seconds or minutes ago, its context is long cancelled,
-- and the relay is a different process on a timer that has never heard of it.
--
-- So the context is captured at write time and carried as data. A column, not a header the
-- relay could invent -- there is nothing left for it to invent from.
--
-- NULLABLE, and it stays nullable. A row written by a code path with no active span -- a
-- backfill, a migration, a test -- is perfectly valid and must not be rejected; it simply
-- produces an event that starts its own trace, which is what happened to every event before
-- this migration.
ALTER TABLE outbox ADD COLUMN trace_parent text;

-- +goose Down

ALTER TABLE outbox DROP COLUMN trace_parent;
