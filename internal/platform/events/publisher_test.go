package events_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/platform/events"
	"github.com/example/gomicro/internal/platform/events/eventstest"
	"github.com/example/gomicro/internal/platform/outbox"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newPublisher wires a Publisher against an embedded server.
func newPublisher(t *testing.T, srv *eventstest.Server, tweak ...func(*config.NATSConfig)) (*events.Publisher, config.Config) {
	t.Helper()

	cfg, err := config.Parse(map[string]string{
		"NATS_URL":     srv.URL,
		"SERVICE_NAME": "orderd",
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	for _, f := range tweak {
		f(&cfg.NATS)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	p, closer, err := events.Connect(ctx, cfg, discard())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(closer)
	return p, cfg
}

func msg(id int64, tenant, eventType string) outbox.Message {
	return outbox.Message{
		ID:          id,
		TenantID:    tenant,
		AggregateID: "11111111-2222-3333-4444-555555555555",
		EventType:   eventType,
		Payload:     []byte(`{"id":"11111111-2222-3333-4444-555555555555"}`),
		OccurredAt:  time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC),
	}
}

// TestTheTenantIsNotInTheSubject is the reason this package puts the tenant in a header.
//
// It is written as a DEMONSTRATION of the alternative rather than an assertion about the
// shipped code, because the shipped code cannot exhibit the bug -- and a design decision whose
// justification is untestable is a design decision nobody can check.
//
// The scenario is not adversarial. "acme.com" is an ordinary tenant id, and any identity
// provider issuing domain-shaped or email-shaped tenants produces one on the first customer.
func TestTheTenantIsNotInTheSubject(t *testing.T) {
	t.Parallel()

	srv := eventstest.Start(t)
	ctx := t.Context()

	if _, err := srv.JS.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "TENANTDEMO",
		Subjects: []string{"demo.>"},
		Storage:  jetstream.MemoryStorage,
	}); err != nil {
		t.Fatalf("stream: %v", err)
	}

	// A consumer built the way every NATS multi-tenancy example shows: filter the tenant's
	// own subtree.
	st, err := srv.JS.Stream(ctx, "TENANTDEMO")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	acmeOnly, err := st.CreateConsumer(ctx, jetstream.ConsumerConfig{
		FilterSubject: "demo.acme.>",
		AckPolicy:     jetstream.AckNonePolicy,
	})
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}

	// Tenant "acme.com" publishing its own event, on the subject layout this package rejected.
	if _, err := srv.JS.Publish(ctx, "demo.acme.com.order.created", []byte(`{"tenant":"acme.com"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	batch, err := acmeOnly.Fetch(1, jetstream.FetchMaxWait(2*time.Second))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var captured []string
	for m := range batch.Messages() {
		captured = append(captured, m.Subject())
	}

	if len(captured) == 0 {
		t.Fatal("the subtree capture did not reproduce.\n\n" +
			"That is not good news: this test exists to justify keeping the tenant OUT of the " +
			"subject, and if NATS has changed its matching rules then that decision should be " +
			"revisited rather than left resting on a test that no longer demonstrates anything.")
	}
	t.Logf("tenant %q was delivered to the %q consumer as %q -- one dot, two tenants",
		"acme.com", "demo.acme.>", captured[0])

	// And now the shipped behaviour: no tenant anywhere in the subject.
	p, cfg := newPublisher(t, srv)
	if err := p.Publish(ctx, msg(1, "acme.com", "order.v1.OrderCreated")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	stored := srv.Messages(t, cfg.NATS.Stream, cfg.NATS.SubjectPrefix+".>")
	if len(stored) != 1 {
		t.Fatalf("stored %d messages, want 1", len(stored))
	}
	if got, want := stored[0].Subject(), "events.order.v1.OrderCreated"; got != want {
		t.Errorf("subject = %q, want %q", got, want)
	}
	if strings.Contains(stored[0].Subject(), "acme") {
		t.Errorf("the tenant reached the subject: %q", stored[0].Subject())
	}
	if got := stored[0].Headers().Get(events.HeaderTenantID); got != "acme.com" {
		t.Errorf("%s = %q, want the tenant to survive intact in the header", events.HeaderTenantID, got)
	}
}

// TestRepublishedEventsAreDeduplicated pins the interlock that makes the relay's rollback cheap.
//
// DrainOnce abandons a whole batch when one publish fails, so messages the broker already
// accepted are published again on the next drain. That is only acceptable because they carry
// the same Nats-Msg-Id and JetStream collapses them. If this stopped working, at-least-once
// would quietly become at-least-twice for every batch that ever hit an error.
func TestRepublishedEventsAreDeduplicated(t *testing.T) {
	t.Parallel()

	srv := eventstest.Start(t)
	p, cfg := newPublisher(t, srv)
	ctx := t.Context()

	m := msg(7, "acme", "order.v1.OrderCreated")
	for range 3 {
		if err := p.Publish(ctx, m); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	if n := srv.Count(t, cfg.NATS.Stream, cfg.NATS.SubjectPrefix+".>"); n != 1 {
		t.Errorf("the stream holds %d copies of outbox row 7, want 1.\n\n"+
			"Nats-Msg-Id deduplication is what makes the relay's whole-batch rollback safe; "+
			"without it every retried batch duplicates every message it had already sent.", n)
	}
}

// TestTheDeduplicationIdIsNamespacedByService catches a silent LOST EVENT.
//
// Nats-Msg-Id is unique per stream, but outbox ids are unique per database and every service's
// outbox starts at 1. Two services sharing a stream would both publish id "1", and JetStream
// would drop the second as a duplicate -- no error, no log, an event that the publisher
// believes it sent and no consumer ever sees.
func TestTheDeduplicationIdIsNamespacedByService(t *testing.T) {
	t.Parallel()

	srv := eventstest.Start(t)
	ctx := t.Context()

	orderd, cfg := newPublisher(t, srv)

	inventoryCfg, err := config.Parse(map[string]string{
		"NATS_URL":     srv.URL,
		"SERVICE_NAME": "inventoryd",
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	inventoryd, closer, err := events.Connect(ctx, inventoryCfg, discard())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(closer)

	// Both services publish THEIR row 1.
	if err := orderd.Publish(ctx, msg(1, "acme", "order.v1.OrderCreated")); err != nil {
		t.Fatalf("orderd publish: %v", err)
	}
	if err := inventoryd.Publish(ctx, msg(1, "acme", "order.v1.OrderCreated")); err != nil {
		t.Fatalf("inventoryd publish: %v", err)
	}

	stored := srv.Messages(t, cfg.NATS.Stream, cfg.NATS.SubjectPrefix+".>")
	if len(stored) != 2 {
		t.Fatalf("the stream holds %d messages, want 2.\n\n"+
			"Two services each published their own outbox row 1. If the id were not "+
			"namespaced by service, the second would be silently deduplicated away -- a lost "+
			"event with no error anywhere.", len(stored))
	}

	ids := []string{
		stored[0].Headers().Get(jetstream.MsgIDHeader),
		stored[1].Headers().Get(jetstream.MsgIDHeader),
	}
	for _, want := range []string{"orderd-1", "inventoryd-1"} {
		if ids[0] != want && ids[1] != want {
			t.Errorf("no message carries Nats-Msg-Id %q; got %v", want, ids)
		}
	}
}

// TestAnOversizedPayloadIsPermanentRatherThanRetriedForever is the failure that would
// otherwise stop the entire outbox.
//
// The NATS server rejects anything over max_payload (1 MiB by default) identically on every
// attempt. Classified as transient, the relay reclaims the row, fails, and never reaches
// anything behind it -- every event in the service blocked by one large one.
func TestAnOversizedPayloadIsPermanentRatherThanRetriedForever(t *testing.T) {
	t.Parallel()

	srv := eventstest.Start(t)
	p, _ := newPublisher(t, srv)

	m := msg(9, "acme", "order.v1.OrderCreated")
	m.Payload = make([]byte, srv.NC.MaxPayload()+1)

	err := p.Publish(t.Context(), m)
	if err == nil {
		t.Fatal("publishing an oversized payload succeeded")
	}
	if !outbox.IsPermanent(err) {
		t.Errorf("publish error is not marked permanent: %v\n\n"+
			"The relay would reclaim this row on every drain and never publish anything "+
			"behind it. Nothing in the logs would say more than the same error, repeating.", err)
	}
}

// TestAMissingStreamFailsRatherThanVanishing is the assertion that the publish is genuinely
// synchronous.
//
// Publishing to a subject no stream captures is perfectly legal in NATS CORE: the message is
// delivered to whoever is subscribed, which is nobody, and the call succeeds. Only the
// JetStream acknowledgement distinguishes "stored" from "went nowhere" -- and the relay marks
// rows published purely on the strength of this return value, so an async publish here would
// convert a deleted stream into silent, permanent data loss.
//
// The scenario is real rather than contrived. Streams get deleted and recreated during
// incidents, and a publisher that had already connected must not keep reporting success.
func TestAMissingStreamFailsRatherThanVanishing(t *testing.T) {
	t.Parallel()

	srv := eventstest.Start(t)
	p, cfg := newPublisher(t, srv)
	ctx := t.Context()

	// It works while the stream is there.
	if err := p.Publish(ctx, msg(1, "acme", "order.v1.OrderCreated")); err != nil {
		t.Fatalf("publish before deletion: %v", err)
	}

	if err := srv.JS.DeleteStream(ctx, cfg.NATS.Stream); err != nil {
		t.Fatalf("delete stream: %v", err)
	}

	err := p.Publish(ctx, msg(2, "acme", "order.v1.OrderCreated"))
	if err == nil {
		t.Fatal("publishing with no stream to store it returned nil.\n\n" +
			"The relay would mark the row published and the event would be gone forever. " +
			"This is exactly what the synchronous ack buys: the async API returns as soon as " +
			"the bytes reach the socket, long before the server has said whether it stored " +
			"anything.")
	}

	// And it must be TRANSIENT: someone will recreate the stream, and the rows waiting in the
	// outbox should then go out. Quarantining them here would set aside events that were
	// always fine.
	if outbox.IsPermanent(err) {
		t.Errorf("a missing stream was classified permanent: %v\n\n"+
			"Recreating the stream fixes it, so the rows must stay claimable.", err)
	}
}

// TestSubjectTokensAreValidated keeps a malformed event type from becoming an unreadable
// broker error.
func TestSubjectTokensAreValidated(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		prefix    string
		eventType string
		wantOK    bool
	}{
		{"ordinary", "events", "order.v1.OrderCreated", true},
		{"hyphen and underscore", "events", "order_v1.Order-Created", true},
		{"empty event type", "events", "", false},
		{"empty token", "events", "order..created", false},
		{"leading dot", "events", ".order.created", false},
		{"trailing dot", "events", "order.created.", false},
		{"wildcard token", "events", "order.*.created", false},
		{"full wildcard", "events", "order.>", false},
		{"space", "events", "order created", false},
		{"wildcard in the prefix", "*", "order.created", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := events.Subject(tc.prefix, tc.eventType)
			switch {
			case tc.wantOK && err != nil:
				t.Errorf("Subject(%q, %q) = error %v, want a subject", tc.prefix, tc.eventType, err)
			case tc.wantOK && got != tc.prefix+"."+tc.eventType:
				t.Errorf("Subject(%q, %q) = %q", tc.prefix, tc.eventType, got)
			case !tc.wantOK && err == nil:
				t.Errorf("Subject(%q, %q) = %q, want an error", tc.prefix, tc.eventType, got)
			case !tc.wantOK && err != nil && !outbox.IsPermanent(err):
				// A bad subject is a code or configuration mistake. Retrying it forever
				// would block the outbox on something no amount of waiting can fix.
				t.Errorf("Subject(%q, %q) failed but not permanently: %v", tc.prefix, tc.eventType, err)
			}
		})
	}
}

// TestTheStreamIsCreatedIdempotently lets every replica call it at startup.
func TestTheStreamIsCreatedIdempotently(t *testing.T) {
	t.Parallel()

	srv := eventstest.Start(t)
	ctx := t.Context()

	cfg, err := config.Parse(map[string]string{"NATS_URL": srv.URL})
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	for i := range 3 {
		if _, err := events.EnsureStream(ctx, srv.JS, cfg.NATS); err != nil {
			t.Fatalf("EnsureStream attempt %d: %v", i+1, err)
		}
	}

	st, err := srv.JS.Stream(ctx, cfg.NATS.Stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	info := st.CachedInfo()

	if info.Config.Duplicates != cfg.NATS.DuplicateWindow {
		t.Errorf("duplicate window = %s, want %s", info.Config.Duplicates, cfg.NATS.DuplicateWindow)
	}

	// The DLQ subtree must be captured by the same stream, or dead-lettering a message
	// fails -- losing exactly the message that was already in trouble.
	var hasDLQ bool
	for _, s := range info.Config.Subjects {
		if strings.HasPrefix(s, cfg.NATS.DLQSubjectPrefix+".") {
			hasDLQ = true
		}
	}
	if !hasDLQ {
		t.Errorf("stream subjects %v do not cover the dead-letter prefix %q",
			info.Config.Subjects, cfg.NATS.DLQSubjectPrefix)
	}
}
