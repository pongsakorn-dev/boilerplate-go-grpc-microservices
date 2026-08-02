package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/platform/outbox"
)

// Publisher sends outbox messages to a JetStream stream. It implements outbox.Publisher.
type Publisher struct {
	js     jetstream.JetStream
	prefix string

	// service namespaces the deduplication id.
	//
	// Nats-Msg-Id is unique per STREAM, but outbox ids are unique per DATABASE and every
	// service's outbox starts at 1. Two services sharing a stream would therefore both
	// publish id "1", and JetStream would silently drop the second as a duplicate -- a lost
	// event, with no error anywhere, discovered eventually by a consumer that never received
	// something the publisher believes it sent.
	service string

	log *slog.Logger
}

// Connect dials NATS, ensures the stream exists, and returns a Publisher and a close function.
//
// The stream is created or updated at startup rather than assumed. A publisher whose stream
// does not exist fails every publish with "no response from stream", which reads like a broker
// outage and is not one.
func Connect(ctx context.Context, cfg config.Config, log *slog.Logger) (*Publisher, func(), error) {
	nc, err := nats.Connect(cfg.NATS.URL,
		nats.Name(cfg.ServiceName),

		// RECONNECT FOREVER. The default gives up after 60 attempts and closes the
		// connection permanently, so a broker outage longer than a minute leaves a worker
		// that is running, healthy by every probe, and publishing nothing ever again.
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.ReconnectJitter(500*time.Millisecond, time.Second),

		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			log.Warn("nats disconnected", "error", errString(err))
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			log.Info("nats reconnected", "url", c.ConnectedUrl())
		}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to nats at %s: %w", cfg.NATS.URL, err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("jetstream: %w", err)
	}

	if _, err := EnsureStream(ctx, js, cfg.NATS); err != nil {
		nc.Close()
		return nil, nil, err
	}

	p := &Publisher{
		js:      js,
		prefix:  cfg.NATS.SubjectPrefix,
		service: cfg.ServiceName,
		log:     log,
	}

	// Drain rather than Close: it flushes what is in flight and waits for the server to
	// acknowledge it, where Close severs the connection and discards anything pending.
	return p, func() {
		if err := nc.Drain(); err != nil {
			log.Warn("nats drain failed", "error", err.Error())
		}
	}, nil
}

// EnsureStream creates or updates the stream. It is idempotent, so every process may call it
// at startup without coordination.
func EnsureStream(ctx context.Context, js jetstream.JetStream, cfg config.NATSConfig) (jetstream.Stream, error) {
	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name: cfg.Stream,

		// The DLQ subtree belongs to the SAME stream on purpose.
		//
		// A separate stream would give dead letters their own retention, which is nicer
		// operationally and is the first thing to change at scale. It also doubles the
		// startup surface, and a dead-letter publish that fails because its stream is missing
		// loses exactly the message that was already in trouble. One stream, one thing to
		// create, and the consumer's filter keeps the two subtrees apart.
		Subjects: []string{cfg.SubjectPrefix + ".>", cfg.DLQSubjectPrefix + ".>"},

		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.FileStorage,

		// Duplicates is the window in which Nats-Msg-Id is remembered. config.Validate
		// refuses a window shorter than the relay's poll interval.
		Duplicates: cfg.DuplicateWindow,
		MaxAge:     cfg.StreamMaxAge,
	})
	if err != nil {
		// The most common failure here is a settings change the server cannot apply in
		// place -- storage type, most often -- against a stream an earlier deploy created.
		// Saying so beats the raw "API error: code=500".
		return nil, fmt.Errorf("create or update stream %q (an existing stream with "+
			"incompatible settings must be migrated or deleted deliberately): %w", cfg.Stream, err)
	}
	return stream, nil
}

// Publish sends one outbox message and waits for JetStream to acknowledge storing it.
//
// SYNCHRONOUS, and that is the load-bearing property. The async API returns a future the
// moment the bytes are written to the socket; using it here would let the relay mark a row
// published before the broker had stored anything, so a broker that died mid-batch would take
// the events with it and the outbox would show them as delivered. Throughput is the reason to
// want async, and it is not worth silently losing events for.
func (p *Publisher) Publish(ctx context.Context, m outbox.Message) error {
	subject, err := Subject(p.prefix, m.EventType)
	if err != nil {
		return err // already marked permanent by Subject
	}

	msg := nats.NewMsg(subject)
	msg.Data = m.Payload
	msg.Header.Set(jetstream.MsgIDHeader, p.msgID(m.ID))
	msg.Header.Set(HeaderTenantID, m.TenantID)
	msg.Header.Set(HeaderEventType, m.EventType)
	msg.Header.Set(HeaderAggregateID, m.AggregateID)
	msg.Header.Set(HeaderOccurredAt, m.OccurredAt.UTC().Format(time.RFC3339Nano))

	ack, err := p.js.PublishMsg(ctx, msg)
	if err != nil {
		return classify(err)
	}

	if ack.Duplicate {
		// Not an error: this is the at-least-once relay meeting the deduplication window and
		// the two behaving exactly as designed. Worth a DEBUG line because a HIGH rate of
		// duplicates means something else is wrong -- usually a marking transaction that
		// keeps failing, which otherwise shows up nowhere.
		p.log.DebugContext(ctx, "jetstream deduplicated a republished event",
			slog.Int64("outbox_id", m.ID), slog.String("subject", subject))
	}
	return nil
}

// JetStream exposes the underlying handle so a consumer can share this connection.
//
// One connection for both directions on purpose: two would double the reconnect logic, the
// monitoring surface and the server's client count, for a worker whose publish and consume
// paths are already in the same process and already fail together.
func (p *Publisher) JetStream() jetstream.JetStream { return p.js }

// msgID namespaces the outbox row id by service. See the field comment on Publisher.service.
func (p *Publisher) msgID(id int64) string {
	return p.service + "-" + strconv.FormatInt(id, 10)
}

// classify decides whether a publish failure is worth retrying.
//
// Only errors that CANNOT succeed on a retry are permanent. Everything else -- no responders,
// timeouts, a severed connection -- is transient, because the relay's correct response to a
// broker problem is to leave the rows unpublished and try again.
func classify(err error) error {
	switch {
	case errors.Is(err, nats.ErrMaxPayload):
		// The one that would otherwise wedge the outbox forever.
		//
		// A payload above the server's max_payload (1 MiB by default) is rejected before it
		// leaves the client, identically, on every attempt. Without this classification the
		// relay would abandon the batch, reclaim the same row, fail again, and never publish
		// anything behind it -- the whole outbox stopped by one large event.
		return Permanent(fmt.Errorf("payload exceeds the server's max_payload: %w", err))

	case errors.Is(err, nats.ErrBadSubject):
		// Rejected by the client before it reaches the wire, so no retry can change it.
		return Permanent(err)

	default:
		return err
	}
}

func errString(err error) string {
	if err == nil {
		// nats calls the disconnect handler with a nil error on a deliberate close.
		return "connection closed"
	}
	return err.Error()
}
