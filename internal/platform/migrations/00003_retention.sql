-- +goose Up

-- THE INDEX THAT MAKES PRUNING CHEAP, and without which it is worse than not pruning.
--
-- cmd/prune deletes published rows older than a cutoff. The relay's claim index cannot serve
-- that query: 00002 redefined it as WHERE published_at IS NULL AND failed_at IS NULL, precisely
-- so the claim never walks either the published pile or the quarantined one.
--
-- Without an index here, each prune batch sequentially scans a table whose whole problem is
-- that it is enormous, on a schedule, forever. The job meant to bound table growth becomes a
-- recurring full-table read. Partial again, and disjoint from the claim index -- but NOT a
-- partition of the table: a quarantined row (published_at NULL, failed_at set) matches neither,
-- which is correct, since neither the relay nor the pruner may touch it. It gets its own index
-- in 00004, for the observer that counts it.
CREATE INDEX outbox_published_idx ON outbox (published_at) WHERE published_at IS NOT NULL;

-- +goose Down

DROP INDEX outbox_published_idx;
