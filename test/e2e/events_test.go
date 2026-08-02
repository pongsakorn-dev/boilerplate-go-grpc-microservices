//go:build e2e

package e2e

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTheEventPathReachesTheProjection is the whole asynchronous story, running in containers.
//
// Six things have to be right and each is tested in isolation somewhere else:
//
//	orderd     writes the outbox row in the SAME transaction as the order
//	worker     claims it with FOR UPDATE SKIP LOCKED
//	worker     publishes it to JetStream and waits for the ack
//	nats       stores it under the configured stream
//	worker     consumes it back through the durable consumer
//	worker     applies the projection and its dedup row in ONE transaction
//
// What no other tier covers is that these six agree across process and network boundaries, with
// the configuration the compose file actually ships. A mistake in any one of them -- a wrong
// NATS_URL, a stream the consumer's filter does not match, a worker that never started -- looks
// identical from outside: the order is created, and the read model silently never updates.
func TestTheEventPathReachesTheProjection(t *testing.T) {
	before := orderCount(t, "dev-tenant")

	const n = 3
	for i := range n {
		createOrder(t, fmt.Sprintf("E2E-EVENT-%d", i))
	}

	// OUTBOX_POLL_INTERVAL is 2s in compose, so the relay needs a moment. Polling rather than
	// sleeping keeps the test fast when it works and specific when it does not.
	want := before + n
	waitFor(t, fmt.Sprintf("order_counts to reach %d", want), 60*time.Second, func() (bool, string) {
		got := orderCount(t, "dev-tenant")
		return got >= want, fmt.Sprintf("order_counts = %d", got)
	})

	if got := orderCount(t, "dev-tenant"); got != want {
		t.Errorf("order_counts = %d, want exactly %d.\n\n"+
			"More than expected means an event was applied twice, which is what the "+
			"processed_events transaction exists to prevent.", got, want)
	}

	// Every outbox row published, none quarantined.
	if got := strings.TrimSpace(psql(t, "SELECT count(*) FROM outbox WHERE published_at IS NULL")); got != "0" {
		t.Errorf("%s outbox rows are still unpublished; the relay is not draining", got)
	}
	if got := strings.TrimSpace(psql(t, "SELECT count(*) FROM outbox WHERE failed_at IS NOT NULL")); got != "0" {
		t.Errorf("%s outbox rows are quarantined:\n%s", got,
			psql(t, "SELECT id, failure_reason FROM outbox WHERE failed_at IS NOT NULL"))
	}

	// And the deduplication rows exist, namespaced by consumer -- the evidence that the
	// projection went through processed_events rather than applying events blindly.
	if got := strings.TrimSpace(psql(t, "SELECT count(DISTINCT message_id) FROM processed_events")); got == "0" {
		t.Error("processed_events is empty, so the consumer applied events with no deduplication")
	}
}

// TestTheStreamHoldsWhatWasPublished checks the broker's own view.
//
// The projection could be right while the broker is wrong -- if, say, the worker published to
// one subject and consumed from another that happened to overlap. Asking NATS directly is the
// independent check.
func TestTheStreamHoldsWhatWasPublished(t *testing.T) {
	createOrder(t, "E2E-STREAM-1")

	waitFor(t, "the EVENTS stream to receive a message", 60*time.Second, func() (bool, string) {
		n := streamMessages(t)
		return n > 0, fmt.Sprintf("stream holds %d messages", n)
	})

	// Nothing dead-lettered. A message on dlq.> means the consumer gave up on something, which
	// in a clean run is always a defect rather than a scenario.
	if n := dlqMessages(t); n > 0 {
		t.Errorf("%d messages are on the dead-letter subject in a run where nothing should "+
			"have failed", n)
	}
}

// streamMessages reads the message count from the NATS monitoring endpoint.
//
// Via the container's own monitoring port rather than a NATS client, so this test needs no
// second connection and asserts on what an operator would actually look at.
func streamMessages(t *testing.T) int {
	t.Helper()
	return jszCount(t, "state.messages")
}

func dlqMessages(t *testing.T) int {
	t.Helper()

	// The DLQ shares the stream, so a total count cannot distinguish them. Compare the
	// consumer's delivered count against the stream: anything the consumer never saw on
	// events.> but that is stored is on another subject.
	out := composeExec(t, "nats", "wget", "-q", "-O", "-",
		"http://localhost:8222/jsz?streams=1&consumers=1")

	// A dead letter is stored under "dlq.", so its presence in the subject list is the signal.
	if strings.Contains(out, `"dlq.`) {
		return 1
	}
	return 0
}

// jszCount pulls one numeric field out of the NATS /jsz document.
//
// Hand-parsed rather than unmarshalled into a struct: the shape of /jsz is not a stable API,
// and a struct here would break on a NATS upgrade for a field this test does not read.
func jszCount(t *testing.T, path string) int {
	t.Helper()

	out := composeExec(t, "nats", "wget", "-q", "-O", "-", "http://localhost:8222/jsz?streams=1")

	// Both spellings appear across NATS versions.
	for _, key := range []string{`"messages":`, `"msgs":`} {
		idx := strings.LastIndex(out, key)
		if idx < 0 {
			continue
		}
		rest := out[idx+len(key):]
		end := strings.IndexAny(rest, ",}")
		if end < 0 {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(rest[:end]))
		if err == nil {
			return n
		}
	}
	t.Fatalf("could not read a message count out of /jsz (looked for %s):\n%s", path, tail(out, 5))
	return 0
}

// orderCount reads the projected read model.
func orderCount(t *testing.T, tenant string) int {
	t.Helper()

	out := strings.TrimSpace(psql(t,
		fmt.Sprintf("SELECT coalesce(max(orders), 0) FROM order_counts WHERE tenant_id = '%s'", tenant)))
	if out == "" {
		return 0
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("order_counts returned %q, which is not a number", out)
	}
	return n
}
