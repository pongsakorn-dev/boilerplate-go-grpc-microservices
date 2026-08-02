package events

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/example/gomicro/internal/platform/config"
)

// Event is one delivered message.
type Event struct {
	// ID is the broker's deduplication key -- the Nats-Msg-Id the publisher set. It is what
	// a Handler records in processed_events, and it is stable across redeliveries.
	ID string

	TenantID    string
	Type        string
	AggregateID string
	Payload     []byte
	OccurredAt  time.Time

	// Deliveries is 1 on first delivery. A Handler can use it to log differently on a retry,
	// but it must not use it to decide whether to apply the effect -- redelivery happens for
	// reasons that have nothing to do with whether the effect landed, and that decision
	// belongs to the deduplication table.
	Deliveries uint64
}

// Handler applies an event.
//
// It must be idempotent. Returning an error marked with Permanent sends the message straight
// to the dead-letter subject; any other error is retried until MaxDeliver is reached.
type Handler interface {
	Handle(ctx context.Context, e Event) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, e Event) error

// Handle calls f.
func (f HandlerFunc) Handle(ctx context.Context, e Event) error { return f(ctx, e) }

// Consumer delivers stream messages to a Handler.
type Consumer struct {
	js      jetstream.JetStream
	cfg     config.NATSConfig
	handler Handler
	log     *slog.Logger
}

// NewConsumer builds a consumer. Run does the work.
func NewConsumer(js jetstream.JetStream, cfg config.NATSConfig, h Handler, log *slog.Logger) *Consumer {
	return &Consumer{js: js, cfg: cfg, handler: h, log: log}
}

// Run consumes until ctx is cancelled, then drains what is already in flight.
func (c *Consumer) Run(ctx context.Context) error {
	stream, err := c.js.Stream(ctx, c.cfg.Stream)
	if err != nil {
		return fmt.Errorf("open stream %q: %w", c.cfg.Stream, err)
	}

	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		// DURABLE. An ephemeral consumer is deleted when the connection drops, so a worker
		// restart would resume from wherever the new consumer's delivery policy starts --
		// replaying the whole stream, or skipping everything that arrived while it was down.
		Durable: c.cfg.Consumer,

		// FilterSubject is what keeps the dead-letter subtree out of this consumer's own
		// input. Without it the consumer would receive the messages it dead-lettered, fail
		// them again, and dead-letter the dead letters. config.Validate refuses a DLQ prefix
		// nested inside this filter for the same reason.
		FilterSubject: c.cfg.SubjectPrefix + ".>",

		// Explicit acks. The alternative policies acknowledge on delivery, which turns any
		// handler failure into a lost message.
		AckPolicy:  jetstream.AckExplicitPolicy,
		MaxDeliver: c.cfg.MaxDeliver,
		AckWait:    c.cfg.AckWait,

		// Backoff between redeliveries, growing. A fixed short retry against a dependency
		// that is down is just a denial of service aimed at your own database.
		//
		// The list is shorter than MaxDeliver on purpose: JetStream reuses the LAST entry for
		// every attempt beyond the list, so this gives 1s, 5s, then 30s forever.
		BackOff: []time.Duration{time.Second, 5 * time.Second, 30 * time.Second},
	})
	if err != nil {
		return fmt.Errorf("create consumer %q: %w", c.cfg.Consumer, err)
	}

	cc, err := cons.Consume(func(msg jetstream.Msg) {
		c.dispatch(ctx, msg)
	})
	if err != nil {
		return fmt.Errorf("consume: %w", err)
	}

	c.log.Info("jetstream consumer started",
		"stream", c.cfg.Stream, "consumer", c.cfg.Consumer,
		"filter", c.cfg.SubjectPrefix+".>", "max_deliver", c.cfg.MaxDeliver)

	<-ctx.Done()

	// Drain, not Stop: Stop discards messages already buffered in the client, which would
	// redeliver them after AckWait -- work thrown away at shutdown and repeated on restart.
	cc.Drain()
	select {
	case <-cc.Closed():
	case <-time.After(c.cfg.AckWait):
		c.log.Warn("jetstream consumer did not drain within AckWait; in-flight messages will be redelivered")
	}

	c.log.Info("jetstream consumer stopped")
	return nil
}

// dispatch runs the handler for one message and decides its fate.
func (c *Consumer) dispatch(ctx context.Context, msg jetstream.Msg) {
	md, err := msg.Metadata()
	if err != nil {
		// Not a JetStream message, so there is nothing to ack against and no metadata to
		// dead-letter with. Nothing useful is possible beyond saying so.
		c.log.ErrorContext(ctx, "received a message with no JetStream metadata", "error", err.Error())
		return
	}

	e := eventFrom(msg, md)

	// The handler gets a context that is NOT the shutdown context.
	//
	// If it were, SIGTERM would cancel a handler mid-transaction, and the message would be
	// redelivered after AckWait -- correct, but noisy, and for a handler that had almost
	// finished. AckWait bounds it instead: the handler has exactly as long as the server is
	// willing to wait before redelivering anyway.
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.AckWait)
	defer cancel()

	err = c.handler.Handle(hctx, e)

	switch {
	case err == nil:
		if ackErr := msg.Ack(); ackErr != nil {
			// The effect landed but the ack did not, so the message will be redelivered.
			// That is exactly what the handler's deduplication is for, and it is worth a
			// warning because a persistent ack failure means a permanently looping consumer.
			c.log.WarnContext(ctx, "handled an event but failed to ack it; it will be redelivered",
				slog.String("event_id", e.ID), slog.String("error", ackErr.Error()))
		}

	case IsPermanent(err):
		// A poison message: unparseable, or referencing something that will never exist.
		// Retrying it MaxDeliver times only delays the same outcome by minutes.
		c.deadLetter(ctx, msg, e, err)

	case e.Deliveries >= uint64(c.cfg.MaxDeliver):
		// THE LAST ATTEMPT, and the reason this branch exists at all.
		//
		// JetStream has no dead-letter queue. Once deliveries exceed MaxDeliver the server
		// simply stops redelivering and emits an advisory that nothing subscribes to by
		// default -- so the message is gone, with no record anywhere the operator looks.
		// Dead-lettering on the final attempt is what turns that silent drop into an
		// inspectable message on a subject with the failure reason attached.
		c.deadLetter(ctx, msg, e, err)

	default:
		c.log.WarnContext(ctx, "event handler failed; will retry",
			slog.String("event_id", e.ID), slog.String("event_type", e.Type),
			slog.Uint64("delivery", e.Deliveries), slog.String("error", err.Error()))

		if nakErr := msg.Nak(); nakErr != nil {
			// Nak failing is harmless -- AckWait expiry redelivers anyway, just later.
			c.log.DebugContext(ctx, "nak failed; redelivery falls back to AckWait",
				slog.String("error", nakErr.Error()))
		}
	}
}

// deadLetter republishes a message under the DLQ prefix and then stops its redelivery.
//
// THE ORDER MATTERS. Terminating first and republishing second would lose the message
// whenever the republish failed -- and a broker unhealthy enough to reject the copy is exactly
// when messages are failing in the first place. So if the copy does not land, the original is
// left alone to be retried rather than terminated.
func (c *Consumer) deadLetter(ctx context.Context, msg jetstream.Msg, e Event, cause error) {
	dlqSubject := c.cfg.DLQSubjectPrefix + "." + msg.Subject()

	copied := nats.NewMsg(dlqSubject)
	copied.Data = msg.Data()
	for k, vals := range msg.Headers() {
		for _, v := range vals {
			copied.Header.Add(k, v)
		}
	}
	// Overwrite rather than add: the original Nats-Msg-Id would deduplicate the dead letter
	// against the live message that is still in the stream, so the copy would never be
	// stored -- and the message would vanish exactly as it would have without a DLQ at all.
	copied.Header.Set(jetstream.MsgIDHeader, "dlq-"+e.ID)
	copied.Header.Set(HeaderDLQReason, cause.Error())
	copied.Header.Set(HeaderDLQDeliveries, strconv.FormatUint(e.Deliveries, 10))

	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if _, err := c.js.PublishMsg(pubCtx, copied); err != nil {
		c.log.ErrorContext(ctx, "could not dead-letter an event; leaving it for redelivery",
			slog.String("event_id", e.ID), slog.String("dlq_subject", dlqSubject),
			slog.String("error", err.Error()))
		_ = msg.Nak()
		return
	}

	c.log.ErrorContext(ctx, "event dead-lettered",
		slog.String("event_id", e.ID), slog.String("event_type", e.Type),
		slog.String("tenant_id", e.TenantID), slog.Uint64("deliveries", e.Deliveries),
		slog.String("dlq_subject", dlqSubject), slog.String("reason", cause.Error()))

	// TermWithReason puts the reason in the server's advisory too, so an operator watching
	// $JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES sees why without opening the DLQ.
	if err := msg.TermWithReason(truncate(cause.Error(), 256)); err != nil {
		c.log.WarnContext(ctx, "dead-lettered an event but failed to terminate the original",
			slog.String("event_id", e.ID), slog.String("error", err.Error()))
	}
}

// eventFrom rebuilds an Event from the wire.
func eventFrom(msg jetstream.Msg, md *jetstream.MsgMetadata) Event {
	h := msg.Headers()

	e := Event{
		ID:          h.Get(jetstream.MsgIDHeader),
		TenantID:    h.Get(HeaderTenantID),
		Type:        h.Get(HeaderEventType),
		AggregateID: h.Get(HeaderAggregateID),
		Payload:     msg.Data(),
		Deliveries:  md.NumDelivered,
	}

	if t, err := time.Parse(time.RFC3339Nano, h.Get(HeaderOccurredAt)); err == nil {
		e.OccurredAt = t
	} else {
		// Fall back to when the stream stored it. A missing or malformed timestamp is not
		// worth failing an event over, but a zero time in a projection is a bug that shows up
		// far from here.
		e.OccurredAt = md.Timestamp
	}

	if e.ID == "" {
		// No Nats-Msg-Id means something published to this stream without going through the
		// relay. The stream sequence is the only stable identifier left, and deduplication
		// needs SOMETHING or the handler would apply it on every redelivery.
		e.ID = fmt.Sprintf("%s-seq-%d", md.Stream, md.Sequence.Stream)
	}

	return e
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
