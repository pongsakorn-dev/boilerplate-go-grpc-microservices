package events_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/platform/events"
	"github.com/example/gomicro/internal/platform/events/eventstest"
	"github.com/example/gomicro/internal/platform/outbox"
)

// recorder captures what a Handler was asked to do.
type recorder struct {
	mu     sync.Mutex
	seen   []events.Event
	notify chan struct{}

	// respond decides the outcome of the Nth call (1-based).
	respond func(n int, e events.Event) error
}

func newRecorder(respond func(n int, e events.Event) error) *recorder {
	return &recorder{notify: make(chan struct{}, 64), respond: respond}
}

func (r *recorder) Handle(_ context.Context, e events.Event) error {
	r.mu.Lock()
	r.seen = append(r.seen, e)
	n := len(r.seen)
	r.mu.Unlock()

	var err error
	if r.respond != nil {
		err = r.respond(n, e)
	}

	select {
	case r.notify <- struct{}{}:
	default:
	}
	return err
}

func (r *recorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

func (r *recorder) events() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.seen...)
}

// waitFor blocks until cond holds or the deadline passes.
func (r *recorder) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.After(20 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-r.notify:
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %s (handler ran %d times)", what, r.calls())
		}
	}
}

// runConsumer starts a consumer in the background and stops it when the test ends.
func runConsumer(t *testing.T, srv *eventstest.Server, cfg config.NATSConfig, h events.Handler) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		if err := events.NewConsumer(srv.JS, cfg, h, discard()).Run(ctx); err != nil {
			t.Errorf("consumer: %v", err)
		}
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("the consumer did not stop within 15s")
		}
	})
}

// publishOne puts a single event on the stream.
func publishOne(t *testing.T, srv *eventstest.Server, id int64, tenant, eventType string, payload []byte) config.Config {
	t.Helper()

	p, cfg := newPublisher(t, srv)
	m := msg(id, tenant, eventType)
	if payload != nil {
		m.Payload = payload
	}
	if err := p.Publish(t.Context(), m); err != nil {
		t.Fatalf("publish: %v", err)
	}
	return cfg
}

// TestHandledEventsAreAckedAndCarryTheirHeaders is the happy path, and it also pins the
// mapping from wire headers to Event fields.
func TestHandledEventsAreAckedAndCarryTheirHeaders(t *testing.T) {
	t.Parallel()

	srv := eventstest.Start(t)
	rec := newRecorder(nil)
	cfg := publishOne(t, srv, 42, "acme.com", "order.v1.OrderCreated", nil)

	runConsumer(t, srv, cfg.NATS, rec)
	rec.waitFor(t, "the event to be handled", func() bool { return rec.calls() >= 1 })

	got := rec.events()[0]
	if got.ID != "orderd-42" {
		t.Errorf("Event.ID = %q, want the namespaced Nats-Msg-Id %q", got.ID, "orderd-42")
	}
	if got.TenantID != "acme.com" {
		t.Errorf("Event.TenantID = %q -- the tenant travels in a header precisely so a dot "+
			"survives it intact", got.TenantID)
	}
	if got.Type != "order.v1.OrderCreated" {
		t.Errorf("Event.Type = %q", got.Type)
	}
	if got.Deliveries != 1 {
		t.Errorf("Event.Deliveries = %d on first delivery, want 1", got.Deliveries)
	}
	if got.OccurredAt.IsZero() {
		t.Error("Event.OccurredAt is zero; a projection would store a meaningless timestamp")
	}

	// Acked means not redelivered. AckWait is 30s by default, so a short wait is enough to
	// show it is not coming back immediately without making the test slow.
	time.Sleep(500 * time.Millisecond)
	if n := rec.calls(); n != 1 {
		t.Errorf("the handler ran %d times for one successfully handled event", n)
	}
}

// TestPoisonMessagesAreDeadLetteredOnTheFirstAttempt keeps a message that can never succeed
// from consuming its whole retry budget first.
func TestPoisonMessagesAreDeadLetteredOnTheFirstAttempt(t *testing.T) {
	t.Parallel()

	srv := eventstest.Start(t)
	rec := newRecorder(func(int, events.Event) error {
		return events.Permanent(errors.New("payload is not valid JSON"))
	})
	cfg := publishOne(t, srv, 1, "acme", "order.v1.OrderCreated", []byte(`{ not json`))

	runConsumer(t, srv, cfg.NATS, rec)
	rec.waitFor(t, "the poison message to be dead-lettered", func() bool {
		return srv.Count(t, cfg.NATS.Stream, cfg.NATS.DLQSubjectPrefix+".>") >= 1
	})

	dlq := srv.Messages(t, cfg.NATS.Stream, cfg.NATS.DLQSubjectPrefix+".>")
	if len(dlq) != 1 {
		t.Fatalf("the dead-letter subject holds %d messages, want 1", len(dlq))
	}

	if got, want := dlq[0].Subject(), "dlq.events.order.v1.OrderCreated"; got != want {
		t.Errorf("dead-letter subject = %q, want %q", got, want)
	}
	if reason := dlq[0].Headers().Get(events.HeaderDLQReason); reason == "" {
		t.Errorf("the dead letter carries no %s header.\n\n"+
			"A DLQ without reasons is a pile of messages nobody can triage.", events.HeaderDLQReason)
	}
	if got := dlq[0].Headers().Get(events.HeaderDLQDeliveries); got != "1" {
		t.Errorf("%s = %q, want 1 -- a poison message must not burn its retry budget first",
			events.HeaderDLQDeliveries, got)
	}

	// Term means no redelivery, ever.
	time.Sleep(time.Second)
	if n := rec.calls(); n != 1 {
		t.Errorf("the handler ran %d times for a permanently failing message, want 1", n)
	}
}

// TestExhaustedRetriesAreDeadLetteredRatherThanDropped covers the silent loss JetStream leaves
// you with by default.
//
// Once deliveries exceed MaxDeliver the server simply stops redelivering. It emits an advisory
// on $JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES that nothing subscribes to unless someone
// built it, so out of the box the message is gone and no log anywhere mentions it.
func TestExhaustedRetriesAreDeadLetteredRatherThanDropped(t *testing.T) {
	t.Parallel()

	srv := eventstest.Start(t)
	rec := newRecorder(func(int, events.Event) error {
		return errors.New("the projection database is down")
	})
	cfg := publishOne(t, srv, 1, "acme", "order.v1.OrderCreated", nil)

	// Three attempts, redelivered fast enough for a test to watch.
	cfg.NATS.MaxDeliver = 3
	cfg.NATS.AckWait = 2 * time.Second

	runConsumer(t, srv, cfg.NATS, rec)
	rec.waitFor(t, "retries to be exhausted and the message dead-lettered", func() bool {
		return srv.Count(t, cfg.NATS.Stream, cfg.NATS.DLQSubjectPrefix+".>") >= 1
	})

	if n := rec.calls(); n != 3 {
		t.Errorf("the handler ran %d times, want MaxDeliver=3", n)
	}

	dlq := srv.Messages(t, cfg.NATS.Stream, cfg.NATS.DLQSubjectPrefix+".>")
	if len(dlq) != 1 {
		t.Fatalf("the dead-letter subject holds %d messages, want 1.\n\n"+
			"Without this the message is simply gone: JetStream stops redelivering and emits "+
			"an advisory nothing is subscribed to.", len(dlq))
	}
	if got := dlq[0].Headers().Get(events.HeaderDLQDeliveries); got != "3" {
		t.Errorf("%s = %q, want 3 -- the count is how you tell an exhausted retry from a "+
			"poison message terminated on sight", events.HeaderDLQDeliveries, got)
	}
	if got := dlq[0].Headers().Get(events.HeaderTenantID); got != "acme" {
		t.Errorf("the dead letter lost its original headers: %s = %q", events.HeaderTenantID, got)
	}
}

// TestDeadLettersAreNotFedBackToTheConsumer prevents the loop that would fill the stream.
//
// The consumer's FilterSubject is what keeps its own dead letters out of its input. Without
// it, every dead letter is redelivered to the consumer that just gave up on it, fails again,
// and is dead-lettered again -- an exponential chain at network speed. config.Validate refuses
// a DLQ prefix nested inside the filter for the same reason; this proves the runtime half.
func TestDeadLettersAreNotFedBackToTheConsumer(t *testing.T) {
	t.Parallel()

	srv := eventstest.Start(t)
	rec := newRecorder(func(int, events.Event) error {
		return events.Permanent(errors.New("always fails"))
	})
	cfg := publishOne(t, srv, 1, "acme", "order.v1.OrderCreated", nil)

	runConsumer(t, srv, cfg.NATS, rec)
	rec.waitFor(t, "the message to be dead-lettered", func() bool {
		return srv.Count(t, cfg.NATS.Stream, cfg.NATS.DLQSubjectPrefix+".>") >= 1
	})

	// If the dead letter came back, the handler would run again and produce a second dead
	// letter, and so on. Two seconds is many round trips on a loopback connection.
	time.Sleep(2 * time.Second)

	// The HANDLER COUNT is checked first, and deliberately.
	//
	// It is an in-process counter, so it reports the truth no matter what state the loop has
	// left the broker in. Asserting on the stream first was tried and was worse: the runaway
	// republishing severs the test's own connection, so the failure arrives as "connection
	// closed" from a helper -- true, useless, and pointing at the wrong file.
	if n := rec.calls(); n != 1 {
		t.Fatalf("the handler ran %d times for one message, want 1.\n\n"+
			"The consumer is being fed its own dead letters: each failure produces a dead "+
			"letter, which is redelivered, which fails, which produces another. Nothing "+
			"stops it, and the stream fills at the speed of the network.", n)
	}

	if n := srv.Count(t, cfg.NATS.Stream, cfg.NATS.DLQSubjectPrefix+".>"); n != 1 {
		t.Errorf("the dead-letter subject holds %d messages, want 1", n)
	}
}

// TestTransientFailuresAreRetriedUntilTheySucceed is the ordinary case: a dependency blips,
// the message comes back, and nothing is lost or duplicated in the broker.
func TestTransientFailuresAreRetriedUntilTheySucceed(t *testing.T) {
	t.Parallel()

	srv := eventstest.Start(t)
	rec := newRecorder(func(n int, _ events.Event) error {
		if n < 3 {
			return errors.New("connection refused")
		}
		return nil
	})
	cfg := publishOne(t, srv, 1, "acme", "order.v1.OrderCreated", nil)
	cfg.NATS.MaxDeliver = 10
	cfg.NATS.AckWait = 2 * time.Second

	runConsumer(t, srv, cfg.NATS, rec)
	rec.waitFor(t, "the third attempt to succeed", func() bool { return rec.calls() >= 3 })

	// It must have stopped there, and nothing should have been dead-lettered.
	time.Sleep(time.Second)
	if n := rec.calls(); n != 3 {
		t.Errorf("the handler ran %d times, want 3 -- it should stop once the event is acked", n)
	}
	if n := srv.Count(t, cfg.NATS.Stream, cfg.NATS.DLQSubjectPrefix+".>"); n != 0 {
		t.Errorf("%d messages were dead-lettered despite eventually succeeding", n)
	}

	deliveries := make([]uint64, 0, 3)
	for _, e := range rec.events() {
		deliveries = append(deliveries, e.Deliveries)
	}
	if len(deliveries) != 3 || deliveries[0] != 1 || deliveries[1] != 2 || deliveries[2] != 3 {
		t.Errorf("Deliveries across attempts = %v, want [1 2 3]", deliveries)
	}
}

// TestAMessageThatCannotBeDeadLetteredIsNotTerminated pins the ordering inside deadLetter.
//
// Terminating first and republishing second is the obvious implementation and it loses
// messages: a broker unhealthy enough to reject the copy is exactly the situation in which
// messages are already failing. Here the dead-letter subject is deliberately outside the
// stream, so the copy cannot be stored -- and the original must survive.
func TestAMessageThatCannotBeDeadLetteredIsNotTerminated(t *testing.T) {
	t.Parallel()

	srv := eventstest.Start(t)
	rec := newRecorder(func(int, events.Event) error {
		return events.Permanent(errors.New("poison"))
	})
	cfg := publishOne(t, srv, 1, "acme", "order.v1.OrderCreated", nil)

	// A prefix no stream captures, so every dead-letter publish fails.
	cfg.NATS.DLQSubjectPrefix = "nowhere"
	cfg.NATS.MaxDeliver = 3
	cfg.NATS.AckWait = 2 * time.Second

	runConsumer(t, srv, cfg.NATS, rec)

	// If the original had been terminated, the handler would have run exactly once.
	rec.waitFor(t, "the message to be redelivered after a failed dead-letter", func() bool {
		return rec.calls() >= 2
	})

	if n := rec.calls(); n < 2 {
		t.Errorf("the handler ran %d times: the message was terminated even though its "+
			"dead letter never landed, so it is gone from everywhere", n)
	}
}

// TestPermanentIsTheOnlyThingThatSkipsRetries guards the classification's default direction.
//
// Treating an unmarked error as permanent would discard events on the first transient blip.
// Treating a permanent one as transient only wastes retries. The default must be the
// recoverable mistake.
func TestPermanentIsTheOnlyThingThatSkipsRetries(t *testing.T) {
	t.Parallel()

	plain := errors.New("boom")
	if outbox.IsPermanent(plain) {
		t.Error("an unmarked error is treated as permanent, so any transient failure would " +
			"quarantine or dead-letter data that was fine")
	}
	if !outbox.IsPermanent(events.Permanent(plain)) {
		t.Error("a marked error is not reported permanent")
	}
	if !errors.Is(events.Permanent(plain), plain) {
		t.Error("marking an error permanent hides it from errors.Is, so callers cannot match " +
			"on the underlying cause")
	}
	if events.Permanent(nil) != nil {
		t.Error("Permanent(nil) must stay nil so it can wrap a call result directly")
	}
}
