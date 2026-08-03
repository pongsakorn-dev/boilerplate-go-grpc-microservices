// Package client dials another gRPC service.
//
// It is the outbound half of the platform, and it is deliberately GENERIC: it hands back a
// configured *grpc.ClientConn and knows nothing about any particular service's protobuf.
// internal/platform must not import internal/order or internal/grpcapi (test/layout_test.go
// enforces it), and that constraint is right -- the day this repository grows a second
// service, this package is reused verbatim rather than forked.
//
// # What it adds over grpc.NewClient
//
//	deadline budget      Reserve headroom so YOUR handler outlives the upstream call and can
//	                     say which dependency was slow. A mesh cannot do this for you.
//	opt-in retries       Per method, never by default, and never on a mutation. See Retry.
//	identity             A service credential, and the caller's tenant as context that
//	                     nothing trusts. The caller's TOKEN is never forwarded -- see
//	                     Credentials for why that is a security decision and not an omission.
//	upstream errors      A callee's status becomes a *client.Error, which is not a
//	                     *status.Error and so cannot be returned to your own callers by
//	                     accident. See Error.
//	tracing              otelgrpc as a StatsHandler, matching the server, so a trace crosses
//	                     the hop.
//	metrics              grpc_client_* labelled by upstream, sharing the server's histogram
//	                     buckets so the two ends of a hop are comparable. Opt-in: see below.
//
// # Wiring the metrics
//
// Options.Metrics is nil by default and publishes nothing. Pass the app's shared registry and
// each upstream gets its own labelled series:
//
//	opts := client.New(cfg, "dns:///inventory:50051")
//	opts.Metrics = app.Metrics()
//	conn, err := client.Dial(cfg, opts)
//
// One line, and without it you can see that your service is slow but not that its upstream is.
// It is opt-in rather than automatic because Dial takes a config.Config, which carries no
// registry -- and a package-global one is exactly the process-wide mutable state
// observability.Metrics exists to avoid.
//
// # It has no production call site yet
//
// Said plainly rather than left to be discovered: nothing in this repository calls another
// service, because the second service was cut from the plan (see the README's roadmap). This
// package is built, tested against a real server over bufconn, and unused. That is a
// deliberate trade -- the client is the reusable part of an inter-service story, and a second
// copy of the order domain would have proved less while costing more -- but a reader deserves
// to know they are looking at a library rather than a live path.
package client

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/stats"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"

	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/platform/observability"
)

// Options configures one upstream connection.
type Options struct {
	// Target is the upstream address, in gRPC target syntax.
	Target string

	// DefaultTimeout bounds a call whose context carries no deadline.
	DefaultTimeout time.Duration

	// ReserveFraction is the slice of the remaining deadline kept back for this service, so
	// an upstream call fails inside your handler rather than alongside it. See budget.
	ReserveFraction float64

	// MinBudget is the least time worth making a call with. Below it the call fails
	// immediately without dialling.
	MinBudget time.Duration

	// Retry lists the methods that may be retried. Empty means none, which is the default
	// and the safe answer for any method that changes state.
	Retry []Retry

	// Credentials is this SERVICE's identity. Nil sends no credential, which is correct for
	// an upstream that does not authenticate and wrong for one that does -- in which case
	// the failure is a clear Unauthenticated from the callee.
	Credentials Credentials

	// TransportCredentials secures the connection. Nil means TLS with the system roots.
	//
	// Insecure is not the default and cannot be reached by omission: it takes Insecure(),
	// which refuses to run under APP_ENV=production.
	TransportCredentials credentials.TransportCredentials

	// DialOptions are appended last, so a caller can add anything this package does not
	// model -- a custom dialer for bufconn, a resolver, a load-balancing policy.
	DialOptions []grpc.DialOption

	// MaxRecvBytes bounds a response. Zero uses the server's own limit from config, so the
	// two ends of a hop agree by construction rather than by coincidence.
	MaxRecvBytes int

	// Metrics publishes grpc_client_* series for this connection, labelled with the upstream.
	//
	// NIL MEANS NO CLIENT METRICS, which is a real choice and not an oversight -- but it is
	// the wrong one for anything long-running. Without these you can see that your service is
	// slow and not that its upstream is, which is the single most common wasted hour in an
	// incident. Pass the app's shared *observability.Metrics; Dial calls ClientFor(Target) so
	// each upstream gets its own labelled series and repeated dials to the same target share
	// one collector rather than panicking on a duplicate registration.
	//
	// Nil is left workable because tests build connections by the dozen and a metrics registry
	// per bufconn is noise.
	Metrics *observability.Metrics
}

// Insecure disables transport security for this connection.
//
// It exists for bufconn and for a local stack, and it is a function rather than a bool field
// so that reading the call site tells you what is happening. Dial refuses it outright when
// APP_ENV=production: an unencrypted service-to-service call carries the service credential in
// clear text, so anything on the path can replay it.
func Insecure() credentials.TransportCredentials { return insecure.NewCredentials() }

// New builds Options from configuration, with this template's defaults applied.
func New(cfg config.Config, target string) Options {
	return Options{
		Target:          target,
		DefaultTimeout:  cfg.Upstream.DefaultTimeout,
		ReserveFraction: cfg.Upstream.ReserveFraction,
		MinBudget:       cfg.Upstream.MinBudget,
		MaxRecvBytes:    cfg.Server.MaxRecvMsgBytes,
	}
}

// Dial connects to the upstream.
//
// The connection is LAZY: grpc.NewClient returns before anything is dialled, so a nil error
// here says the target parsed and the options are valid, and says nothing whatever about
// whether the upstream exists. Do not treat a successful Dial as a readiness check.
func Dial(cfg config.Config, opts Options) (*grpc.ClientConn, error) {
	if opts.Target == "" {
		return nil, fmt.Errorf("client: no target")
	}
	opts.applyDefaults()

	creds := opts.TransportCredentials
	switch {
	case creds == nil:
		// TLS with the host's root pool. A template that defaulted to insecure would have
		// every fork inherit it, and the one that noticed would be the one that had already
		// shipped.
		creds = credentials.NewTLS(nil)

	case cfg.IsProduction() && creds == insecure.NewCredentials():
		return nil, fmt.Errorf(
			"client: refusing an insecure connection to %s when APP_ENV=production: the "+
				"service credential would cross the network in clear text", opts.Target)
	}

	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),

		// otelgrpc as a StatsHandler, matching the server. The interceptor form is deprecated
		// and blind to streaming, and mixing the two forms across a hop produces a trace with
		// a gap exactly where the hop is.
		//
		// Built here rather than at package level because observability.NewTracerProvider
		// installs the global propagator, and a handler constructed before that call captures
		// the no-op one permanently.
		grpc.WithStatsHandler(otelgrpc.NewClientHandler(
			otelgrpc.WithFilter(func(info *stats.RPCTagInfo) bool {
				// The same filter the server uses. A client with grpc-go's built-in health
				// checking enabled emits a Check every few seconds per connection, which is
				// the span volume the server-side filter exists to kill, re-created outbound.
				return observability.FilterHealthChecks(info.FullMethodName)
			}),
		)),

		grpc.WithChainUnaryInterceptor(opts.unaryChain()...),
		grpc.WithChainStreamInterceptor(opts.streamChain()...),

		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(opts.MaxRecvBytes)),

		// Keepalive must stay WEAKER than the server's enforcement policy. The server sets
		// GRPC_KEEPALIVE_MIN_TIME and PermitWithoutStream=false; a client that pings faster,
		// or pings with no active stream, is answered with GOAWAY ENHANCE_YOUR_CALM and the
		// connection is torn down -- which looks exactly like an upstream outage.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                cfg.Server.KeepaliveMinTime * 2,
			Timeout:             20 * time.Second,
			PermitWithoutStream: false,
		}),
	}

	serviceConfigJSON, err := buildServiceConfig(opts.Retry, true)
	if err != nil {
		return nil, err
	}
	if serviceConfigJSON != "" {
		dialOpts = append(dialOpts, grpc.WithDefaultServiceConfig(serviceConfigJSON))
	}

	dialOpts = append(dialOpts, opts.DialOptions...)

	conn, err := grpc.NewClient(opts.Target, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("client: dial %s: %w", opts.Target, err)
	}
	return conn, nil
}

// unaryChain assembles the outbound interceptors, outermost first.
//
// METRICS GO OUTERMOST, and the position is load-bearing rather than aesthetic.
//
// The budget interceptor can fail a call BEFORE it reaches the wire -- when too little of the
// caller's deadline remains to be worth dialling, it returns DeadlineExceeded without making a
// request at all. Placed below metrics, those refusals are counted, which is what you want:
// from the caller's point of view the upstream call failed, and a dashboard that showed only
// the calls that were actually attempted would hide an upstream so slow that this service had
// stopped trying to reach it -- the exact failure the budget exists to create.
func (c *Options) unaryChain() []grpc.UnaryClientInterceptor {
	chain := []grpc.UnaryClientInterceptor{}
	if c.Metrics != nil {
		chain = append(chain, c.Metrics.ClientFor(c.Target).UnaryClientInterceptor())
	}
	return append(chain, c.unaryIdentity(), c.unaryBudget(), c.unaryErrors())
}

func (c *Options) streamChain() []grpc.StreamClientInterceptor {
	chain := []grpc.StreamClientInterceptor{}
	if c.Metrics != nil {
		chain = append(chain, c.Metrics.ClientFor(c.Target).StreamClientInterceptor())
	}
	return append(chain, c.streamIdentity(), c.streamBudget())
}

// applyDefaults fills the zero values so a hand-built Options is still safe.
func (c *Options) applyDefaults() {
	if c.DefaultTimeout <= 0 {
		c.DefaultTimeout = 10 * time.Second
	}
	if c.ReserveFraction <= 0 || c.ReserveFraction >= 1 {
		c.ReserveFraction = 0.1
	}
	if c.MinBudget <= 0 {
		c.MinBudget = 50 * time.Millisecond
	}
	if c.MaxRecvBytes <= 0 {
		c.MaxRecvBytes = 4 << 20
	}
}

// unaryIdentity attaches credentials and caller context.
func (c *Options) unaryIdentity() grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		ctx, err := c.attachIdentity(ctx)
		if err != nil {
			return fmt.Errorf("client: credentials for %s: %w", method, err)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// streamIdentity is the streaming twin.
func (c *Options) streamIdentity() grpc.StreamClientInterceptor {
	return func(
		ctx context.Context,
		desc *grpc.StreamDesc,
		cc *grpc.ClientConn,
		method string,
		streamer grpc.Streamer,
		opts ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		ctx, err := c.attachIdentity(ctx)
		if err != nil {
			return nil, fmt.Errorf("client: credentials for %s: %w", method, err)
		}
		return streamer(ctx, desc, cc, method, opts...)
	}
}

// unaryErrors converts an upstream status into a *client.Error.
//
// INNERMOST of the three, so it wraps what the upstream actually returned rather than what a
// higher interceptor made of it -- and so a budget failure, which never reached the wire,
// keeps its own shape instead of being relabelled as an upstream fault.
func (c *Options) unaryErrors() grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		return newUpstreamError(c.Target, method, invoker(ctx, method, req, reply, cc, opts...))
	}
}
