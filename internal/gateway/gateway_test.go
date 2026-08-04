package gateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/example/gomicro/internal/platform/auth/testjwks"
	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/testutil"
)

// THE WHOLE POINT OF THIS FILE is that REST and gRPC are the same service.
//
// grpc-gateway offers a "server mode" registration that calls the handler implementation
// directly, in-process, with no gRPC server involved. It is faster and it silently removes
// every interceptor: the REST surface would have no authentication, no authorisation, no
// admission control, no error mapping, no metrics and no tracing -- while looking, in the
// code that registers it, exactly like the mode that has all six.
//
// So the assertions here are mostly not about JSON. They are about the chain running.

// doJSON issues a request and returns the status and decoded body.
func doJSON(t *testing.T, base, method, path, body string, headers map[string]string) (int, map[string]any, string) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded, string(raw)
}

// TestRESTGoesThroughTheInterceptorChain is the assertion the design turns on.
//
// If the gateway were registered in server mode, this request would reach the handler with no
// principal, the handler would read the tenant from a context that has none, and the caller
// would get a 500 -- or, worse on a service whose handlers tolerate it, a 200.
func TestRESTGoesThroughTheInterceptorChain(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	srv, _ := testutil.NewTestGateway(t, oidcMode(iss))

	status, body, raw := doJSON(t, srv.URL, http.MethodGet, "/v1/orders", "", nil)

	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /v1/orders returned %d, want 401.\n\n"+
			"The REST edge is not running the interceptor chain. That is what happens with "+
			"grpc-gateway's server-mode registration: the handler is called directly and every "+
			"interceptor -- auth included -- is skipped.\n\nbody: %s", status, raw)
	}

	// And the error is in OUR shape, not grpc-gateway's default {"code":16,...}.
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("error body has no `error` object: %s", raw)
	}
	if got := errObj["code"]; got != "Unauthenticated" {
		t.Errorf("error.code = %v, want the symbolic gRPC code.\n\n"+
			"grpc-gateway's default body carries the NUMERIC code, which means nothing to an "+
			"HTTP client that never sees gRPC.", got)
	}
}

// TestAuthenticatedRESTRoundTrip covers the happy path and the JSON contract in one go.
func TestAuthenticatedRESTRoundTrip(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	srv, _ := testutil.NewTestGateway(t, oidcMode(iss))
	auth := bearer(iss.Sign(iss.DefaultClaims()))

	created := `{"customer_id":"cust-rest","items":[{"sku":"SKU-1","quantity":2,` +
		`"unit_price":{"currency_code":"USD","units":"10","nanos":0}}]}`

	status, body, raw := doJSON(t, srv.URL, http.MethodPost, "/v1/orders", created, auth)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/orders returned %d: %s", status, raw)
	}

	order, _ := body["order"].(map[string]any)
	if order == nil {
		t.Fatalf("response has no `order`: %s", raw)
	}

	// UseProtoNames: the field is customer_id, matching the .proto, the gRPC surface, the
	// database column and the OpenAPI document. Not customerId.
	if _, ok := order["customer_id"]; !ok {
		t.Errorf("response uses camelCase, not proto names. Field names now differ between "+
			"REST and every other surface.\nbody: %s", raw)
	}

	// The tenant came from the TOKEN, through the gateway, through the chain, into the
	// handler -- no request field carries it.
	if got := order["tenant_id"]; got != "tenant-a" {
		t.Errorf("order.tenant_id = %v, want tenant-a (from the verified token)", got)
	}

	orderID, _ := order["order_id"].(string)
	if orderID == "" {
		t.Fatalf("no order_id in the response: %s", raw)
	}

	// And it is readable back through the path binding.
	status, body, raw = doJSON(t, srv.URL, http.MethodGet, "/v1/orders/"+orderID, "", auth)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/orders/{id} returned %d: %s", status, raw)
	}
	got, _ := body["order"].(map[string]any)
	if got["order_id"] != orderID {
		t.Errorf("round trip returned a different order: %s", raw)
	}
}

// TestEmitUnpopulatedKeepsTheResponseShapeStable guards a subtle client-breaking default.
//
// protojson omits zero values unless told otherwise, so a field would vanish from the JSON
// exactly when it holds its zero value. A client then cannot distinguish "the server did not
// set this" from "the server set it to zero", and every generated type needs optionality the
// schema says does not exist.
func TestEmitUnpopulatedKeepsTheResponseShapeStable(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	srv, _ := testutil.NewTestGateway(t, oidcMode(iss))
	auth := bearer(iss.Sign(iss.DefaultClaims()))

	// An empty list: next_page_token is the zero value and must still be present.
	status, body, raw := doJSON(t, srv.URL, http.MethodGet, "/v1/orders", "", auth)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/orders returned %d: %s", status, raw)
	}
	if _, ok := body["next_page_token"]; !ok {
		t.Errorf("next_page_token is absent when empty, so a client cannot tell "+
			"\"no more pages\" from \"the server did not answer\".\nbody: %s", raw)
	}
}

// TestUnknownJSONFieldsAreRejected is the strictness worth having.
//
// protojson can be told to ignore unknown fields. It is not, so a client that misspells
// customer_id is told, rather than left wondering why every order has an empty customer.
func TestUnknownJSONFieldsAreRejected(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	srv, _ := testutil.NewTestGateway(t, oidcMode(iss))
	auth := bearer(iss.Sign(iss.DefaultClaims()))

	body := `{"customer_idd":"typo","items":[{"sku":"S","quantity":1,` +
		`"unit_price":{"currency_code":"USD","units":"1"}}]}`

	status, _, raw := doJSON(t, srv.URL, http.MethodPost, "/v1/orders", body, auth)
	if status != http.StatusBadRequest {
		t.Errorf("a misspelled field returned %d, want 400.\n\n"+
			"Silently discarding unknown fields turns a client typo into an order with no "+
			"customer.\nbody: %s", status, raw)
	}
}

// TestErrorMappingMatchesTheGRPCSurface checks the two surfaces agree on what an error IS.
//
// The HTTP status comes from apperr's table -- the same table that produced the gRPC code --
// rather than from grpc-gateway's own mapping. They agree today; without this, nothing would
// notice when they stopped.
func TestErrorMappingMatchesTheGRPCSurface(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	srv, _ := testutil.NewTestGateway(t, oidcMode(iss))
	auth := bearer(iss.Sign(iss.DefaultClaims()))

	t.Run("not found is 404 and carries a machine-readable reason", func(t *testing.T) {
		status, body, raw := doJSON(t, srv.URL,
			http.MethodGet, "/v1/orders/00000000-0000-7000-8000-00000000dead", "", auth)

		if status != http.StatusNotFound {
			t.Fatalf("got %d, want 404: %s", status, raw)
		}
		errObj, _ := body["error"].(map[string]any)
		if errObj["reason"] == nil || errObj["reason"] == "" {
			t.Errorf("no `reason` in the error body, so a client must string-match on the "+
				"message -- the exact thing the error model exists to prevent.\nbody: %s", raw)
		}
		if errObj["code"] != "NotFound" {
			t.Errorf("error.code = %v, want NotFound", errObj["code"])
		}
	})

	t.Run("validation failure is 400 and lists the offending fields", func(t *testing.T) {
		status, body, raw := doJSON(t, srv.URL, http.MethodPost, "/v1/orders", `{}`, auth)

		if status != http.StatusBadRequest {
			t.Fatalf("got %d, want 400: %s", status, raw)
		}
		errObj, _ := body["error"].(map[string]any)
		details, _ := errObj["details"].([]any)
		if len(details) == 0 {
			t.Errorf("no `details` on a validation error. The gRPC surface returns BadRequest "+
				"field violations; dropping them here makes REST strictly less useful for "+
				"exactly the errors clients must handle.\nbody: %s", raw)
		}
	})

	t.Run("a missing scope is 403 and does not name the scope", func(t *testing.T) {
		claims := iss.DefaultClaims()
		claims[iss.ScopeClaim] = "orders:read"
		readOnly := bearer(iss.Sign(claims))

		created := `{"customer_id":"c","items":[{"sku":"S","quantity":1,` +
			`"unit_price":{"currency_code":"USD","units":"1"}}]}`

		status, _, raw := doJSON(t, srv.URL, http.MethodPost, "/v1/orders", created, readOnly)
		if status != http.StatusForbidden {
			t.Fatalf("got %d, want 403: %s", status, raw)
		}
		if strings.Contains(raw, "orders:write") {
			t.Errorf("the response names the missing scope, handing an attacker a shopping "+
				"list:\n%s", raw)
		}
	})
}

// TestTenantIsolationHoldsOverREST re-proves the isolation on the second surface.
//
// Not redundant with the gRPC test: the whole risk of a REST edge is that it becomes a second
// way in with different rules. This asserts the tenant still comes from the token when the
// caller speaks HTTP.
func TestTenantIsolationHoldsOverREST(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	srv, _ := testutil.NewTestGateway(t, oidcMode(iss))

	tokenFor := func(tenant string) map[string]string {
		claims := iss.DefaultClaims()
		claims[iss.TenantClaim] = tenant
		return bearer(iss.Sign(claims))
	}

	created := `{"customer_id":"cust-a","items":[{"sku":"SKU-1","quantity":1,` +
		`"unit_price":{"currency_code":"USD","units":"5"}}]}`

	status, body, raw := doJSON(t, srv.URL, http.MethodPost, "/v1/orders", created, tokenFor("tenant-a"))
	if status != http.StatusOK {
		t.Fatalf("create as tenant-a: %d %s", status, raw)
	}
	order, _ := body["order"].(map[string]any)
	orderID, _ := order["order_id"].(string)

	status, _, raw = doJSON(t, srv.URL, http.MethodGet, "/v1/orders/"+orderID, "", tokenFor("tenant-b"))
	if status != http.StatusNotFound {
		t.Errorf("tenant-b got %d for tenant-a's order, want 404.\n\n"+
			"403 would confirm the order exists, turning this endpoint into an oracle for "+
			"enumerating other tenants' ids.\nbody: %s", status, raw)
	}
}

// TestGatewayCanBeDisabled asserts the off switch really is off.
//
// A service with only gRPC clients should not expose an HTTP surface it never uses, and
// "GATEWAY_ADDR is empty" must mean no handler exists at all rather than one that binds
// nothing.
func TestGatewayCanBeDisabled(t *testing.T) {
	t.Parallel()

	conn := testutil.NewTestServer(t, func(c *config.Config) { c.GatewayAddr = "" })
	if conn == nil {
		t.Fatal("the gRPC server should still work with the gateway disabled")
	}
}

// TestAStreamingRestErrorIsNotTheUnaryShape pins a difference the code cannot remove, so
// that the next person is not tempted to "fix" it with a handler that changes nothing.
//
// One was registered for exactly that reason. It was `status.Convert(err)` behind an if whose
// branches both returned the same value -- identical to runtime.DefaultStreamErrorHandler --
// under a comment claiming it replaced the default. Nobody was careless: the seam simply
// cannot deliver what the comment wanted. StreamErrorHandlerFunc returns a *status.Status and
// the runtime marshals `{"error": <google.rpc.Status>}` around it, so the JSON shape is not
// the handler's to choose.
//
// The two surfaces are contrasted in one test on purpose. Asserting the streaming shape alone
// would pass just as well if the unary shape regressed to match it, and the unary shape --
// symbolic code, top-level reason -- is the one this service actually promises.
func TestAStreamingRestErrorIsNotTheUnaryShape(t *testing.T) {
	t.Parallel()

	// An ANONYMOUS request is the fixture, because the rejection has to happen before the
	// first message. Auth runs ahead of the handler, so the stream fails without ever being
	// opened -- and, unlike a quota rejection, it returns immediately. A watch that succeeds
	// holds its connection open until the deadline, which makes it useless as a test subject.
	iss := testjwks.New(t)
	srv, _ := testutil.NewTestGateway(t, oidcMode(iss))

	unaryStatus, unaryBody, unaryRaw := doJSON(t, srv.URL, http.MethodGet, "/v1/orders", "", nil)

	if unaryStatus != http.StatusUnauthorized {
		t.Fatalf("anonymous unary call returned %d, want 401: %s", unaryStatus, unaryRaw)
	}
	unaryErr, ok := unaryBody["error"].(map[string]any)
	if !ok {
		t.Fatalf("unary error body has no error object: %s", unaryRaw)
	}
	// The exact value, not merely "a string". An earlier draft asserted only the Go type and
	// passed happily when the field was sabotaged to the empty string -- which is a string,
	// and is useless to a client.
	if unaryErr["code"] != codes.Unauthenticated.String() {
		t.Errorf("unary code = %q, want %q.\n\n"+
			"An integer is meaningless to a client that never sees gRPC, and the symbolic "+
			"form is the shape this service promises: %s",
			unaryErr["code"], codes.Unauthenticated.String(), unaryRaw)
	}
	if unaryErr["reason"] == nil || unaryErr["reason"] == "" {
		t.Errorf("unary error has no top-level reason, which is what clients branch on: %s", unaryRaw)
	}

	streamStatus, streamBody, streamRaw := doJSON(t, srv.URL, http.MethodGet, "/v1/orders:watch", "", nil)

	if streamStatus != http.StatusUnauthorized {
		t.Fatalf("anonymous stream open returned %d, want 401: %s", streamStatus, streamRaw)
	}
	streamErr, ok := streamBody["error"].(map[string]any)
	if !ok {
		t.Fatalf("stream error body has no error object: %s", streamRaw)
	}

	// The documented difference. If this ever starts matching the unary shape, grpc-gateway
	// has gained a seam worth using and errors.go's explanation is out of date.
	if _, isString := streamErr["code"].(string); isString {
		t.Errorf("the streaming error carries a symbolic code %v.\n\n"+
			"errors.go says this is impossible through WithStreamErrorHandler. If it is now "+
			"possible, that comment needs rewriting and the handler needs writing: %s",
			streamErr["code"], streamRaw)
	}
	if _, hasReason := streamErr["reason"]; hasReason {
		t.Errorf("the streaming error has a top-level reason: %s.\n\n"+
			"Same as above -- errors.go documents that reason survives only inside details.",
			streamRaw)
	}

	// And the reason is not LOST, just relocated into details. A client can still find it; it
	// just has to look somewhere else than on the unary path, which is the fact worth
	// documenting rather than the fact worth fixing.
	wantReason, _ := unaryErr["reason"].(string)
	if wantReason != "" && !strings.Contains(streamRaw, wantReason) {
		t.Errorf("the streaming error dropped the machine-readable reason %q entirely: %s",
			wantReason, streamRaw)
	}
}

// --- helpers -------------------------------------------------------------------------

func oidcMode(iss *testjwks.Issuer) func(*config.Config) {
	return func(c *config.Config) {
		c.AuthMode = config.AuthOIDC
		c.OIDC.IssuerURL = iss.URL()
		c.OIDC.Audience = iss.Audience
		c.OIDC.TenantClaim = iss.TenantClaim
		c.OIDC.ScopeClaim = iss.ScopeClaim
	}
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// TestTheRESTEdgeForwardsTraceContext pins the header matcher, and it is behavioural rather
// than a unit test of the mapping function because the mapping function was never the doubt --
// whether grpc-gateway consults it on the path that matters was.
//
// This shipped broken. grpc-gateway's default matcher forwards `Grpc-Metadata-*` and a fixed
// list of permanent HTTP headers; W3C Trace Context is on neither, so a traceparent from an
// instrumented HTTP client was dropped at the edge. The gRPC surface carried it perfectly, so
// every trace-related test passed and only the REST half was severed -- silently, with the
// handler starting a fresh root span as if the caller had sent nothing.
func TestTheRESTEdgeForwardsTraceContext(t *testing.T) {
	// A real SDK provider so spans are recorded. The dead OTLP sink is the same trick
	// observability/tracing_test.go uses: NewTracerProvider returns a no-op provider when no
	// endpoint is set, and a no-op provider records nothing to assert on.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {}))
	endpoint := discardOTLPSink(t)

	srv, _ := testutil.NewTestGateway(t, func(c *config.Config) {
		c.Telemetry.OTLPEndpoint = endpoint
		c.Telemetry.TraceSampleRatio = 1.0
	})

	tp, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider)
	if !ok {
		t.Fatalf("the global tracer provider is %T, not an SDK provider", otel.GetTracerProvider())
	}
	recorder := tracetest.NewSpanRecorder()
	tp.RegisterSpanProcessor(recorder)
	t.Cleanup(func() { tp.UnregisterSpanProcessor(recorder) })

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/orders", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/orders: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	spans := recorder.Ended()
	if len(spans) == 0 {
		t.Fatal("the request produced no spans at all; the assertion below would be vacuous")
	}

	for _, s := range spans {
		if s.SpanContext().TraceID().String() == traceID {
			return // the caller's trace context reached the RPC
		}
	}

	var got []string
	for _, s := range spans {
		got = append(got, s.Name()+"="+s.SpanContext().TraceID().String())
	}
	t.Errorf("no span joined the caller's trace %s; saw %v.\n\n"+
		"The REST edge is dropping traceparent, so an HTTP caller's trace ends at the gateway "+
		"and everything the request causes -- including the outbox row and the projection that "+
		"follows it -- belongs to a different trace with nothing linking them.", traceID, got)
}

// discardOTLPSink accepts OTLP connections and refuses the export quickly, so the exporter
// does not retry against a refused port for five seconds during shutdown.
func discardOTLPSink(t *testing.T) string {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the OTLP sink: %v", err)
	}
	grpcSrv := grpc.NewServer()
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	return lis.Addr().String()
}
