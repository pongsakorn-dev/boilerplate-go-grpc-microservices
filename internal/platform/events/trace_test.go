package events_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/example/gomicro/internal/platform/events"
	"github.com/example/gomicro/internal/platform/events/eventstest"
)

// The outbox is the ONE hop in this system where the producer and the consumer are separated
// by time rather than by a network.
//
// Everywhere else the trace rides in-band -- gRPC metadata, an HTTP header, something the
// caller holds while the callee runs. Here the request that caused the event returned long
// before the relay looked at the row, its context is cancelled, and the worker is a different
// process on a timer. Without the trace being stored as data and replayed here, every
// asynchronous effect in the service begins a brand-new trace, and the question "why is the
// read model stale for this order?" has no path from the order to the answer.

// TestTheTraceSurvivesTheBroker is the assertion the whole column exists for.
func TestTheTraceSurvivesTheBroker(t *testing.T) {
	t.Parallel()

	srv := eventstest.Start(t)

	// A real trace, created the way a request would.
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	ctx, span := tp.Tracer("test").Start(context.Background(), "the causing request")
	want := span.SpanContext().TraceID().String()
	span.End()

	// What the store adapter would have written into outbox.trace_parent.
	traceParent := traceparentOf(ctx)
	if traceParent == "" {
		t.Fatal("could not render a traceparent from a sampled span; the fixture is wrong")
	}

	seen := make(chan string, 4)
	p, cfg := newPublisher(t, srv)
	runConsumer(t, srv, cfg.NATS, events.HandlerFunc(func(hctx context.Context, _ events.Event) error {
		// What the HANDLER sees. This is the whole point: the projection's own work has to
		// land on the same trace as the request that caused it.
		seen <- trace.SpanContextFromContext(hctx).TraceID().String()
		return nil
	}))

	m := msg(1, "acme", "order.created")
	m.TraceParent = traceParent
	if err := p.Publish(context.Background(), m); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got := <-seen
	if got != want {
		t.Errorf("the handler ran under trace %s, want %s.\n\n"+
			"The trace did not survive the broker, so the asynchronous half of every request "+
			"is a separate trace with nothing linking it back. That is the difference between "+
			"'the projection is slow' and 'THIS order's projection is slow'.", got, want)
	}
}

// TestAnEventWithNoTraceIsStillDelivered covers the ordinary case, and it is a case rather
// than a footnote because it is the one that breaks if the header handling is careless.
//
// Rows written outside any span -- a backfill, a migration, a test -- carry an empty
// trace_parent. The publisher must omit the header entirely rather than send an empty one:
// W3C traceparent has a required shape, and an empty value is not "no trace", it is a
// malformed one that a strict propagator may reject or, worse, treat as a valid-but-invalid
// parent that silently swallows the span.
func TestAnEventWithNoTraceIsStillDelivered(t *testing.T) {
	t.Parallel()

	srv := eventstest.Start(t)

	delivered := make(chan struct{}, 4)
	p, cfg := newPublisher(t, srv)
	runConsumer(t, srv, cfg.NATS, events.HandlerFunc(func(context.Context, events.Event) error {
		delivered <- struct{}{}
		return nil
	}))

	m := msg(1, "acme", "order.created") // TraceParent deliberately empty
	if err := p.Publish(context.Background(), m); err != nil {
		t.Fatalf("publishing an untraced event failed: %v", err)
	}

	<-delivered

	// And the header really is absent rather than present-and-empty.
	stored := srv.Messages(t, "EVENTS", "events.>")
	if len(stored) == 0 {
		t.Fatal("nothing was stored")
	}
	if _, present := stored[0].Headers()[events.HeaderTraceParent]; present {
		t.Errorf("an empty traceparent header was published.\n\n"+
			"W3C traceparent has a required shape; %q is not a valid one. Omit the header "+
			"instead -- an absent header is unambiguously 'no trace'.",
			stored[0].Headers().Get(events.HeaderTraceParent))
	}
}

// TestTheDeadLetterKeepsItsTrace is the case where the trace matters most.
//
// A dead-lettered event is one somebody will be investigating, and the first question is what
// caused it. deadLetter copies every header from the original, which preserves traceparent for
// free -- this pins that behaviour, because a future rewrite that copies a named subset of
// headers would drop exactly this one and nothing else would notice.
func TestTheDeadLetterKeepsItsTrace(t *testing.T) {
	t.Parallel()

	srv := eventstest.Start(t)

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	ctx, span := tp.Tracer("test").Start(context.Background(), "the causing request")
	span.End()
	traceParent := traceparentOf(ctx)

	// A handler that refuses permanently, so the message is dead-lettered on the first try.
	rec := newRecorder(func(int, events.Event) error {
		return events.Permanent(errors.New("unprocessable"))
	})
	p, cfg := newPublisher(t, srv)
	runConsumer(t, srv, cfg.NATS, rec)

	m := msg(1, "acme", "order.created")
	m.TraceParent = traceParent
	if err := p.Publish(context.Background(), m); err != nil {
		t.Fatalf("publish: %v", err)
	}

	rec.waitFor(t, "the event to be dead-lettered", func() bool {
		return srv.Count(t, "EVENTS", "dlq.>") > 0
	})

	dlq := srv.Messages(t, "EVENTS", "dlq.>")
	if got := dlq[0].Headers().Get(events.HeaderTraceParent); got != traceParent {
		t.Errorf("the dead letter carries traceparent %q, want %q.\n\n"+
			"A dead-lettered event is the one somebody investigates, and the first question "+
			"is what caused it. Without the trace there is no way back to the request.",
			got, traceParent)
	}
}

// traceparentOf renders a context's span as a W3C traceparent, mirroring what the store
// adapter writes into the outbox row.
func traceparentOf(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}
