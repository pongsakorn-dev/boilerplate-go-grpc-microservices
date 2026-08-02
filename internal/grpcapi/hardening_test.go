package grpcapi_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/testutil"
)

// The four hardening options in chain.go were CONFIGURED and never PROVEN.
//
// grpc.MaxConcurrentStreams, grpc.MaxRecvMsgSize and the two keepalive policies are DoS
// controls, and a DoS control is a claim about what happens under conditions no ordinary
// test creates. Every other test in this package sends one small message on an idle
// connection, so all four options could be deleted and nothing in the suite would notice.
//
// That is not hypothetical for this repository. The M5 auth bypass was a correct verifier
// that the server never called, and it survived precisely because the tests exercised the
// component rather than the wiring. These options have the same shape: a value read from
// config, passed to a constructor, and never observed again.
//
// All four assertions below drive the PRODUCTION server through testutil.NewTestServer, and
// each is paired with a control that isolates the option under test -- a limit that rejects
// everything proves nothing.

// TestAMessageOverTheLimitIsRejectedBeforeAnyInterceptorRuns pins grpc.MaxRecvMsgSize.
//
// Without it, grpc-go's default is 4 MiB, and a fork that raises it "to be safe" allocates
// whatever a caller sends before a single line of application code runs.
//
// The sharp assertion is the CODE, not the failure. customer_id is capped at 64 characters by
// a protovalidate rule, so an oversized request is invalid on two independent grounds. If
// validation ran first the answer would be InvalidArgument; ResourceExhausted is only
// reachable if the transport rejected the frame before the interceptor chain was entered.
// One RPC therefore proves both that the limit exists and that it sits below everything else.
func TestAMessageOverTheLimitIsRejectedBeforeAnyInterceptorRuns(t *testing.T) {
	t.Parallel()

	const limit = 1024

	conn := testutil.NewTestServer(t, func(c *config.Config) {
		c.Server.MaxRecvMsgBytes = limit
	})
	client := orderv1.NewOrderServiceClient(conn)

	// CONTROL FIRST. A valid, small request must succeed at this same limit.
	//
	// Without it the test passes just as well against a server that rejects everything --
	// including one whose limit was misconfigured to zero, which is the likeliest way this
	// option actually breaks.
	if _, err := client.CreateOrder(context.Background(), validCreateRequest("small")); err != nil {
		t.Fatalf("a small valid request failed at a %d-byte limit: %v\n\n"+
			"The limit is rejecting traffic it should carry, so the assertion below would "+
			"pass for the wrong reason.", limit, err)
	}

	req := validCreateRequest(strings.Repeat("x", limit*4))

	_, err := client.CreateOrder(context.Background(), req)
	switch got := status.Code(err); got {
	case codes.ResourceExhausted:
		// What we want.
	case codes.InvalidArgument:
		t.Fatalf("an oversized request returned InvalidArgument, not ResourceExhausted.\n\n"+
			"That means protovalidate rejected it on customer_id's max_len rule -- so the "+
			"message was fully received and decoded, and grpc.MaxRecvMsgSize did NOT apply. "+
			"A caller can still make this server allocate an arbitrarily large buffer; only "+
			"the error text changed.\nerr: %v", err)
	default:
		t.Fatalf("an oversized request returned %v, want ResourceExhausted: %v", got, err)
	}
}

// TestConcurrentStreamsAreCapped pins grpc.MaxConcurrentStreams.
//
// grpc-go's default is math.MaxUint32 -- effectively unbounded -- so one client can open
// enough concurrent streams to exhaust the server's memory. This repo ships exactly one
// streaming RPC and it holds its stream open until the caller goes away, which makes the
// exhaustion trivially reachable rather than theoretical.
//
// The cap is observed through a UNARY call, which is the part worth noticing: an HTTP/2
// stream is an HTTP/2 stream, so streams held open by a watch starve ordinary request
// traffic on the same connection. That coupling is invisible until it happens in production.
func TestConcurrentStreamsAreCapped(t *testing.T) {
	t.Parallel()

	const limit = 2

	conn := testutil.NewTestServer(t, func(c *config.Config) {
		c.Server.MaxConcurrentStreams = limit
	})
	client := orderv1.NewOrderServiceClient(conn)

	streamCtx, releaseStreams := context.WithCancel(context.Background())
	defer releaseStreams()

	// Occupy every slot, and PROVE each stream is really established server-side by reading
	// a message from it. Merely calling WatchOrders returns as soon as the client has queued
	// the stream, so counting calls would let this test pass with the cap removed.
	//
	// The memory store is seeded at startup (app.go), so the handler has something to send.
	for i := range limit {
		stream, err := client.WatchOrders(streamCtx, &orderv1.WatchOrdersRequest{})
		if err != nil {
			t.Fatalf("opening stream %d: %v", i, err)
		}
		if _, err := stream.Recv(); err != nil {
			t.Fatalf("stream %d never produced a message, so it is not established "+
				"server-side and holds no slot: %v", i, err)
		}
	}

	// Every slot is now held. A unary call has nowhere to run and must time out waiting.
	blocked, cancelBlocked := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelBlocked()

	_, err := client.ListOrders(blocked, &orderv1.ListOrdersRequest{})
	if got := status.Code(err); got != codes.DeadlineExceeded {
		t.Fatalf("a unary call with all %d stream slots held returned %v, want "+
			"DeadlineExceeded: %v\n\n"+
			"The cap is not being applied. grpc-go's default MaxConcurrentStreams is "+
			"effectively unbounded, so a client holding open watches can allocate server "+
			"memory without limit.", limit, got, err)
	}

	// CONTROL. Release the streams and the identical call must now succeed.
	//
	// This is what distinguishes "the cap blocked it" from "the server is wedged", and it is
	// not a formality: a first draft of this test held the streams with a context that was
	// never cancelled, so the server stayed saturated and this call failed too -- which read
	// as a passing cap either way.
	releaseStreams()

	free, cancelFree := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelFree()

	if _, err := client.ListOrders(free, &orderv1.ListOrdersRequest{}); err != nil {
		t.Fatalf("the same call failed after the stream slots were released: %v\n\n"+
			"So the timeout above was not caused by the stream cap, and this test proves "+
			"nothing about MaxConcurrentStreams.", err)
	}
}

// TestTheConfiguredKeepalivePolicyIsWhatTheTransportEnforces pins
// grpc.KeepaliveEnforcementPolicy, and it speaks raw HTTP/2 rather than using grpc-go's
// client. Both of those choices were forced, and the reasons are the useful part of this test.
//
// WHY NOT grpc.WithKeepaliveParams. It silently clamps: `if kp.Time < 10s { kp.Time = 10s }`
// in dialoptions.go, logged at warning level and otherwise invisible. The server needs
// pingStrikes > maxPingStrikes (2), so a real client needs four pings to be cut off -- about
// forty seconds. The first version of this test asked for 50ms pings, waited fifteen seconds,
// saw a healthy connection, and reported that the enforcement policy was missing. It was not;
// the request for 50ms had been rewritten to 10s on the way past.
//
// WHY THE ASSERTION IS PERMISSIVE-ALLOWS RATHER THAN STRICT-DENIES. A ping flood on a
// connection with NO open stream is refused by grpc-go's DEFAULT policy too, because the
// default is also PermitWithoutStream:false. A test built on that flood passes identically
// with the whole grpc.KeepaliveEnforcementPolicy option deleted -- proving nothing about this
// repository's configuration. The pair below is the smallest thing that does not have that
// defect: an identical flood, on an identical open stream, at an identical rate, against two
// servers that differ only in KeepaliveMinTime. Deleting the option restores the 5-minute
// default and turns the permissive case red.
func TestTheConfiguredKeepalivePolicyIsWhatTheTransportEnforces(t *testing.T) {
	t.Parallel()

	// Ten pings 20ms apart: far faster than the 5-minute default, far slower than a 1ms
	// MinTime. That gap is the whole experiment.
	const pings = 10
	const interval = 20 * time.Millisecond

	t.Run("a permissive MinTime lets the pings through", func(t *testing.T) {
		t.Parallel()

		if err := pingFlood(t, time.Millisecond, pings, interval); err != nil {
			t.Fatalf("a server configured with MinTime=1ms cut off a client pinging every "+
				"%s: %v\n\n"+
				"KeepaliveMinTime is not reaching the transport, so the value in config is "+
				"decorative and the server is enforcing grpc-go's 5-minute default instead. "+
				"A fork that deliberately loosens this to support aggressive clients would "+
				"find its clients disconnected anyway, with nothing to explain why.",
				interval, err)
		}
	})

	t.Run("a strict MinTime cuts the connection", func(t *testing.T) {
		t.Parallel()

		err := pingFlood(t, time.Hour, pings, interval)
		if err == nil {
			t.Fatal("a server configured with MinTime=1h tolerated a client pinging every " +
				"20ms.\n\nThe enforcement policy is absent, so one connection can burn " +
				"server CPU with pings for free -- the cheapest denial of service there is, " +
				"because a ping costs the attacker nothing and needs no stream.")
		}
		if !strings.Contains(err.Error(), "too_many_pings") {
			t.Errorf("the connection ended with %v, which does not mention too_many_pings.\n\n"+
				"Something other than the keepalive policy closed it, so this is not "+
				"evidence the policy works.", err)
		}
	})
}

// pingFlood opens one real gRPC stream over a raw HTTP/2 connection, sends count pings spaced
// by interval, and reports the GOAWAY if the server sent one.
//
// THE STREAM IS NOT INCIDENTAL. grpc-go's server picks its ping accounting branch on whether
// any stream is active: with none, it checks PermitWithoutStream and a fixed timeout and never
// consults MinTime at all. Pinging an idle connection would therefore exercise a code path
// this repository does not configure. The stream is opened, half-closed by the client, and
// left open by the handler (WatchOrders blocks until its context ends), which is what keeps
// the server on the MinTime branch for the duration.
func pingFlood(t *testing.T, minTime time.Duration, count int, interval time.Duration) error {
	t.Helper()

	lis := testutil.NewTestServerListener(t, func(c *config.Config) {
		c.Server.KeepaliveMinTime = minTime
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := lis.DialContext(ctx)
	if err != nil {
		t.Fatalf("dial the in-memory listener: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
		t.Fatalf("write the HTTP/2 client preface: %v", err)
	}

	fr := http2.NewFramer(conn, conn)

	// http2.Framer is not safe for concurrent writes, and there are two writers here: this
	// goroutine sending pings, and the reader below acknowledging the server's SETTINGS.
	var writeMu sync.Mutex
	write := func(fn func() error) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return fn()
	}

	if err := write(func() error { return fr.WriteSettings() }); err != nil {
		t.Fatalf("write SETTINGS: %v", err)
	}

	openWatchStream(t, fr, write)

	established := make(chan struct{})
	closed := make(chan error, 1)
	go readFrames(fr, write, established, closed)

	// Do not start pinging until the server has actually begun the RPC. Pinging earlier would
	// land while activeStreams is still zero, which is the branch this test is specifically
	// not about.
	select {
	case <-established:
	case err := <-closed:
		t.Fatalf("the connection closed before the stream was established: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("the server never responded on the stream, so the RPC never started")
	}

	for range count {
		if err := write(func() error { return fr.WritePing(false, [8]byte{}) }); err != nil {
			// The server has already hung up; whatever the reader saw is the real answer.
			break
		}
		select {
		case err := <-closed:
			return err
		case <-time.After(interval):
		}
	}

	// A short grace period: the GOAWAY may still be in flight behind the last ping.
	select {
	case err := <-closed:
		return err
	case <-time.After(time.Second):
		return nil
	}
}

// openWatchStream sends the HEADERS and DATA of a WatchOrders call on stream 1.
//
// Hand-encoded because the point is to control ping timing, which grpc-go's client will not
// surrender. AUTH_MODE is dev in the test config, so no credential metadata is required --
// under AUTH_MODE=oidc this would need an authorization header.
func openWatchStream(t *testing.T, fr *http2.Framer, write func(func() error) error) {
	t.Helper()

	var block bytes.Buffer
	enc := hpack.NewEncoder(&block)
	for _, f := range []hpack.HeaderField{
		{Name: ":method", Value: "POST"},
		{Name: ":scheme", Value: "http"},
		{Name: ":path", Value: "/order.v1.OrderService/WatchOrders"},
		{Name: ":authority", Value: "bufnet"},
		{Name: "content-type", Value: "application/grpc"},
		// grpc-go's server rejects a request without this outright, as a defence against
		// HTTP/2 proxies replaying a gRPC call as ordinary HTTP.
		{Name: "te", Value: "trailers"},
	} {
		if err := enc.WriteField(f); err != nil {
			t.Fatalf("hpack encode %s: %v", f.Name, err)
		}
	}

	err := write(func() error {
		return fr.WriteHeaders(http2.HeadersFrameParam{
			StreamID:      1,
			BlockFragment: block.Bytes(),
			EndHeaders:    true,
		})
	})
	if err != nil {
		t.Fatalf("write HEADERS: %v", err)
	}

	// One empty gRPC message: a five-byte length-prefixed frame carrying a zero-length
	// WatchOrdersRequest, with END_STREAM to half-close the client side. That is exactly what
	// a server-streaming call looks like on the wire.
	if err := write(func() error { return fr.WriteData(1, true, []byte{0, 0, 0, 0, 0}) }); err != nil {
		t.Fatalf("write DATA: %v", err)
	}
}

// readFrames drains the connection, closing established on the first response for stream 1 and
// reporting on closed when the server goes away.
func readFrames(fr *http2.Framer, write func(func() error) error, established chan<- struct{}, closed chan<- error) {
	var once sync.Once
	markEstablished := func() { once.Do(func() { close(established) }) }

	for {
		f, err := fr.ReadFrame()
		if err != nil {
			closed <- fmt.Errorf("connection closed: %w", err)
			return
		}

		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				_ = write(fr.WriteSettingsAck)
			}
		case *http2.GoAwayFrame:
			closed <- fmt.Errorf("GOAWAY code=%v debug=%q", f.ErrCode, f.DebugData())
			return
		case *http2.HeadersFrame:
			if f.StreamID == 1 {
				markEstablished()
			}
		case *http2.DataFrame:
			if f.StreamID == 1 {
				markEstablished()
			}
		}
	}
}

// validCreateRequest builds a request that passes every protovalidate rule, so a rejection
// can only come from somewhere else.
func validCreateRequest(customerID string) *orderv1.CreateOrderRequest {
	return &orderv1.CreateOrderRequest{
		CustomerId: customerID,
		Items: []*orderv1.OrderItem{{
			Sku:       "SKU-1",
			Quantity:  1,
			UnitPrice: &orderv1.Money{CurrencyCode: "USD", Units: 10, Nanos: 0},
		}},
	}
}
