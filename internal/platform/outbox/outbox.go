// Package outbox drains the transactional outbox to a message broker.
//
// THE GUARANTEES, stated precisely, because every one of them is a thing people assume
// wrongly about outboxes:
//
//	✓ No lost events.       The outbox row is written by the SAME transaction as the business
//	                        change, so "the order committed but the event vanished" is not a
//	                        state the database can hold. internal/order/ordertest asserts this
//	                        against real Postgres.
//	✓ At-least-once.        Every event reaches the broker at least once.
//	✗ NOT exactly-once.     If the broker accepts a publish and the marking transaction then
//	                        fails, the event is republished. Consumers MUST deduplicate --
//	                        that is what Nats-Msg-Id and the processed_events table are for in
//	                        M8b. Anyone who tells you they have exactly-once delivery has
//	                        moved the deduplication somewhere you have not looked yet.
//	✗ NOT globally ordered. Two relay instances claim disjoint batches concurrently, so event
//	                        20 can reach the broker before event 19.
//	~ Per-aggregate order    holds only with a SINGLE relay instance. Two relays can split two
//	                        events for the same order across batches and finish out of order.
//	                        A fork that needs strict per-aggregate ordering runs one relay, or
//	                        shards claims by hash(aggregate_id).
//
// Those last two are the ones that bite. They are written here rather than discovered.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
)

// Message is one row of the outbox, ready to publish.
type Message struct {
	// ID is the outbox row id, monotonic per writer. It doubles as the broker-side
	// deduplication key: publishing it as Nats-Msg-Id (M8b) is what turns at-least-once
	// delivery into effectively-once processing.
	ID int64

	TenantID    string
	AggregateID string
	EventType   string

	// Payload is the JSON snapshot written by the domain. Bytes rather than a decoded type,
	// because the relay is deliberately ignorant of what it is shipping -- decoding here
	// would couple a generic relay to every event schema in the service.
	Payload []byte

	OccurredAt time.Time
}

// Publisher sends a message onward.
//
// The one seam between the relay and the broker. internal/platform/events implements it over
// NATS JetStream, and nothing in the relay changed when it did.
type Publisher interface {
	// Publish delivers m, or returns an error.
	//
	// An error marked with Permanent means retrying is pointless and the row is quarantined.
	// Any other error leaves the row unpublished for the next drain.
	Publish(ctx context.Context, m Message) error
}

// permanent marks an error that retrying cannot fix.
//
// It lives here, next to Publisher, because it is part of that interface's contract rather
// than a detail of any one implementation: the relay has to decide between "try again in a
// second" and "set this row aside", and only the publisher knows which is which.
//
// A wrapper rather than a sentinel, so the underlying error survives errors.Is and errors.As.
type permanent struct{ err error }

func (p permanent) Error() string { return p.err.Error() }
func (p permanent) Unwrap() error { return p.err }

// Permanent marks err as unretryable. It returns nil for a nil error.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanent{err: err}
}

// IsPermanent reports whether retrying err is pointless.
//
// Everything unmarked is treated as transient. That default is deliberate and the asymmetry
// is the point: calling a transient failure permanent quarantines an event that would have
// gone through, while calling a permanent failure transient only wastes retries until someone
// looks. The second mistake is visible and reversible; the first is neither.
func IsPermanent(err error) bool {
	var p permanent
	return errors.As(err, &p)
}

// LogPublisher writes events to the log instead of a broker.
//
// A PLACEHOLDER, and named so nobody mistakes it for a transport. It exists because the hard
// parts of this milestone -- claiming rows without double-delivery, batching, the marking
// transaction, at-least-once semantics -- are all testable without a broker, and shipping
// them behind an unimplemented interface would mean shipping code nothing runs.
//
// It is also genuinely useful while developing: `go run ./cmd/worker` shows exactly what
// would be published, with no infrastructure at all.
type LogPublisher struct {
	Log *slog.Logger
}

// Publish logs the event.
func (p LogPublisher) Publish(ctx context.Context, m Message) error {
	// Compacted so one event is one log line. A pretty-printed payload turns a busy relay's
	// output into something no aggregator can index usefully.
	var compact []byte
	if json.Valid(m.Payload) {
		compact = m.Payload
	}

	p.Log.InfoContext(ctx, "outbox event (LogPublisher: not sent to any broker)",
		slog.Int64("outbox_id", m.ID),
		slog.String("event_type", m.EventType),
		slog.String("aggregate_id", m.AggregateID),
		slog.String("tenant_id", m.TenantID),
		slog.Time("occurred_at", m.OccurredAt),
		slog.Int("payload_bytes", len(compact)))
	return nil
}
