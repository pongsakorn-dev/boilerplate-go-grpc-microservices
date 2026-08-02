-- +goose Up

-- A PARTIAL INDEX OVER ROWS THAT ARE NORMALLY ABSENT, which is what makes it nearly free.
--
-- outbox.Observer counts quarantined rows on a timer so an operator can alert on them. In a
-- healthy service that count is zero, and it must stay cheap to ask -- a sequential scan of the
-- whole outbox every fifteen seconds would cost far more than the problem it watches for.
--
-- Partial on failed_at IS NOT NULL means the index contains only quarantined rows: usually no
-- entries at all, so it occupies almost nothing and is not touched by ordinary INSERTs. It
-- becomes non-empty exactly when something has gone wrong, which is the moment the query needs
-- to be fast rather than the moment it starts costing.
CREATE INDEX outbox_quarantined_idx ON outbox (failed_at) WHERE failed_at IS NOT NULL;

-- +goose Down

DROP INDEX outbox_quarantined_idx;
