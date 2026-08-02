-- +goose Up

-- THE INDEX THAT MAKES PRUNING CHEAP, and without which it is worse than not pruning.
--
-- cmd/prune deletes published rows older than a cutoff. The existing partial index covers only
-- the UNPUBLISHED rows -- it is built WHERE published_at IS NULL, precisely so the relay's claim
-- never walks the published pile -- so it cannot serve this query at all.
--
-- Without an index here, each prune batch sequentially scans a table whose whole problem is
-- that it is enormous, on a schedule, forever. The job meant to bound table growth becomes a
-- recurring full-table read. Partial again, and mirroring the claim index: these two together
-- cover the table with no overlap, because a row is either published or it is not.
CREATE INDEX outbox_published_idx ON outbox (published_at) WHERE published_at IS NOT NULL;

-- +goose Down

DROP INDEX outbox_published_idx;
