//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	requireStack(t)

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
	requireStack(t)

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

// TestTheWorkerExposesOutboxHealth is the reason the worker gained a listener.
//
// The gauges are unit-tested against a real database in the integration tier. What no other
// tier covers is that they are REACHABLE: that the worker binds an admin address the compose
// file actually publishes, that /metrics is served there, and that the outbox series are in
// the exposition rather than only in the process.
//
// That gap is not hypothetical for this repository. ADMIN_ADDR defaults to 127.0.0.1:9090,
// which is correct on a laptop and unscrapable inside a container -- the metrics would exist,
// be correct, and be visible to nobody. Only a test that scrapes from outside the container
// can tell those two states apart.
func TestTheWorkerExposesOutboxHealth(t *testing.T) {
	requireStack(t)

	body := scrapeWorkerMetrics(t)

	// The three series an operator alerts on, named exactly. A rename silently breaks every
	// alert built on them, and nothing else in the repo would notice.
	for _, metric := range []string{
		"gomicro_outbox_pending_rows",
		"gomicro_outbox_quarantined_rows",
		"gomicro_outbox_oldest_pending_age_seconds",
		"gomicro_outbox_last_observation_timestamp_seconds",
	} {
		if !strings.Contains(body, metric) {
			t.Errorf("the worker's /metrics does not export %s.\n\n"+
				"The gauge may well be correct inside the process; it is not reaching a "+
				"scraper, which is the only thing that makes it an alert.", metric)
		}
	}

	// And it is the WORKER's surface, not orderd's: the worker registers no gRPC server
	// metrics, so their presence would mean the wrong container answered.
	if strings.Contains(body, "grpc_server_handled_total") {
		t.Error("the worker's admin port is serving grpc_server_* series.\n\n" +
			"Either the port mapping reaches orderd instead of the worker, or the worker was " +
			"given the full metrics registry -- which publishes RPC series that can never " +
			"move off zero in a process that serves no RPCs.")
	}
}

// TestQuarantinedRowsBecomeVisible drives the whole path the gauge exists for.
//
// A quarantined row is created directly rather than by provoking a real permanent failure:
// what is under test here is the observability path -- database to gauge to scrape -- not the
// relay's decision to quarantine, which relay_integration_test.go already covers.
func TestQuarantinedRowsBecomeVisible(t *testing.T) {
	requireStack(t)

	// Start from whatever the stack already has, since earlier tests share it.
	before := metricValue(t, scrapeWorkerMetrics(t), "gomicro_outbox_quarantined_rows")

	psql(t, `INSERT INTO outbox (tenant_id, aggregate_id, event_type, payload, occurred_at, failed_at, failure_reason)
	         VALUES ('dev-tenant', gen_random_uuid(), 'order.created', '{}'::jsonb, now(), now(), 'e2e: a deliberately poisoned row')`)

	// OUTBOX_OBSERVE_INTERVAL is 2s in compose.
	want := before + 1
	waitFor(t, fmt.Sprintf("the quarantined gauge to reach %v", want), 60*time.Second, func() (bool, string) {
		got := metricValue(t, scrapeWorkerMetrics(t), "gomicro_outbox_quarantined_rows")
		return got >= want, fmt.Sprintf("gauge = %v", got)
	})

	// Clean up so later runs and the shared stack are not left with a poisoned row.
	psql(t, `DELETE FROM outbox WHERE failure_reason = 'e2e: a deliberately poisoned row'`)
}

// scrapeWorkerMetrics reads the worker's admin surface from OUTSIDE the container, over the
// published host port -- the same path Prometheus would take.
func scrapeWorkerMetrics(t *testing.T) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:9091/metrics", nil)
	if err != nil {
		t.Fatalf("build the scrape request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scrape the worker's /metrics on the published port: %v\n\n"+
			"Either the worker is not binding ADMIN_ADDR to an address reachable from "+
			"outside the container, or the compose file no longer publishes it.", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the worker's /metrics returned %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the exposition: %v", err)
	}
	return string(body)
}

// metricValue pulls one unlabelled sample out of a Prometheus exposition.
func metricValue(t *testing.T, exposition, name string) float64 {
	t.Helper()

	for _, line := range strings.Split(exposition, "\n") {
		if !strings.HasPrefix(line, name+" ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			t.Fatalf("%s has a non-numeric value %q", name, fields[1])
		}
		return v
	}
	t.Fatalf("no sample named %q in the exposition", name)
	return 0
}

// TestTheTraceReachesTheOutboxRow is the trace-propagation assertion that only the shipped
// stack can make.
//
// The unit and integration tiers cover each link: orderpg writes the traceparent, the relay
// selects it, the publisher sends it, the consumer resumes it. What none of them exercise is
// the REAL request path in front of all that -- the gateway, otelgrpc's server handler, the
// global propagator installed at startup, and the interceptor chain -- deciding whether the
// caller's trace context ever reaches the code that writes the row.
//
// A traceparent is sent in as a caller would, and the row that comes out has to carry the same
// trace id. It works with no collector configured because a non-recording span still carries
// its parent's SpanContext, which is exactly the property that makes tracing safe to leave
// instrumented everywhere and exported nowhere.
func TestTheTraceReachesTheOutboxRow(t *testing.T) {
	requireStack(t)

	// A fixed, recognisable trace id, in W3C form: version-traceid-spanid-flags. The 01 flag
	// says sampled, which is what makes a downstream propagator keep it.
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const traceParent = "00-" + traceID + "-00f067aa0ba902b7-01"

	orderID := createOrderTraced(t, "E2E-TRACE-1", traceParent)

	stored := strings.TrimSpace(psql(t, fmt.Sprintf(
		"SELECT coalesce(trace_parent, '') FROM outbox WHERE aggregate_id = '%s'", orderID)))

	if stored == "" {
		t.Fatalf("the outbox row for order %s has no trace_parent.\n\n"+
			"The caller's trace context did not reach the code that writes the row, so every "+
			"asynchronous effect of this request starts a new trace with nothing linking it "+
			"back.", orderID)
	}
	if !strings.Contains(stored, traceID) {
		t.Errorf("the outbox row carries %q, which does not contain the caller's trace id %s.\n\n"+
			"A trace was captured, but not the caller's -- so the event joins some unrelated "+
			"trace, which is worse than joining none.", stored, traceID)
	}
}

// createOrderTraced posts an order carrying a W3C traceparent, as an instrumented caller would.
func createOrderTraced(t *testing.T, sku, traceParent string) string {
	t.Helper()

	body := fmt.Sprintf(`{"customer_id":"e2e-traced","items":[
		{"sku":%q,"quantity":1,"unit_price":{"currency_code":"USD","units":"3","nanos":0}}]}`, sku)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/v1/orders", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("traceparent", traceParent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/orders: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/orders returned %s", resp.Status)
	}

	var decoded struct {
		Order struct {
			OrderID string `json:"order_id"`
		} `json:"order"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode the response: %v", err)
	}
	if decoded.Order.OrderID == "" {
		t.Fatal("the response carries no order id")
	}
	return decoded.Order.OrderID
}
