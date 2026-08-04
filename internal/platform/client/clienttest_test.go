package client_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/example/gomicro/internal/platform/client"
	"github.com/example/gomicro/internal/platform/config"
)

// upstream is a controllable callee.
//
// It is built with grpc.UnknownServiceHandler rather than a generated service, which is what
// makes it able to answer ANY method name without a .proto -- so these tests can name
// "order.v1.OrderService/GetOrder" and have it behave however the case needs. What is under
// test here is the CLIENT; a callee that can be told to fail twice and then succeed, or to
// report exactly what deadline and metadata it received, is the instrument.
//
// (The hop against the real production server is a separate test -- see
// TestARealServerAcceptsWhatThisClientSends.)
type upstream struct {
	t   *testing.T
	lis *bufconn.Listener

	mu       sync.Mutex
	calls    []observed
	respond  func(n int) error
	blockFor time.Duration
}

// observed is what the callee saw on one call.
type observed struct {
	method      string
	md          metadata.MD
	deadline    time.Duration // remaining, as measured on arrival
	hadDeadline bool
}

func newUpstream(t *testing.T, respond func(n int) error) *upstream {
	t.Helper()

	u := &upstream{t: t, lis: bufconn.Listen(1024 * 1024), respond: respond}

	srv := grpc.NewServer(grpc.UnknownServiceHandler(u.handle))
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(u.lis)
	}()

	t.Cleanup(func() {
		srv.Stop()
		<-served
		_ = u.lis.Close()
	})

	return u
}

func (u *upstream) handle(_ any, stream grpc.ServerStream) error {
	ctx := stream.Context()
	method, _ := grpc.Method(ctx)

	obs := observed{method: method}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		obs.md = md.Copy()
	}
	if deadline, ok := ctx.Deadline(); ok {
		obs.hadDeadline = true
		obs.deadline = time.Until(deadline)
	}

	u.mu.Lock()
	u.calls = append(u.calls, obs)
	n := len(u.calls)
	respond, block := u.respond, u.blockFor
	u.mu.Unlock()

	// Drain the request so the transport is in a sane state before answering.
	var req emptypb.Empty
	if err := stream.RecvMsg(&req); err != nil {
		return err
	}

	if block > 0 {
		select {
		case <-time.After(block):
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}

	if respond != nil {
		if err := respond(n); err != nil {
			// Returned WITHOUT sending headers first, deliberately: grpc-go abandons a retry
			// policy the moment response headers are on the wire, so a callee that calls
			// SetHeader before failing silently makes every retry test vacuous.
			return err
		}
	}

	return stream.SendMsg(&emptypb.Empty{})
}

func (u *upstream) observedCalls() []observed {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]observed(nil), u.calls...)
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

func (u *upstream) blockEachCallFor(d time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.blockFor = d
}

// testConfig is a valid development configuration.
func testConfig(t *testing.T) config.Config {
	t.Helper()

	cfg, err := config.Parse(map[string]string{"APP_ENV": config.EnvDevelopment})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

// dial builds a client against the fake upstream.
func (u *upstream) dial(t *testing.T, mutate ...func(*client.Options)) *grpc.ClientConn {
	t.Helper()

	cfg := testConfig(t)

	opts := client.New(cfg, "passthrough:///upstream")
	opts.TransportCredentials = client.Insecure()
	opts.DialOptions = []grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return u.lis.DialContext(ctx)
		}),
	}
	for _, fn := range mutate {
		fn(&opts)
	}

	conn, err := client.Dial(cfg, opts)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// invoke makes one unary call.
func invoke(ctx context.Context, conn *grpc.ClientConn, method string) error {
	return conn.Invoke(ctx, "/"+method, &emptypb.Empty{}, &emptypb.Empty{})
}

// unavailableTimes fails the first n attempts with Unavailable, then succeeds.
func unavailableTimes(n int) func(int) error {
	return func(attempt int) error {
		if attempt <= n {
			return status.Error(codes.Unavailable, "upstream restarting")
		}
		return nil
	}
}

// There is deliberately NO codeOf(err) = status.Code(err) helper here any more.
//
// It existed, and it worked on the *client.Error values this package returns -- which it
// could only do by reaching through them to the upstream's status. That is the leak
// client.Error exists to prevent, made to look like ordinary test usage. Read the upstream's
// code from the typed field instead: client.From(err) then .Code.
