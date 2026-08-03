// Package gateway is the JSON/REST edge.
//
// It transcodes HTTP+JSON onto the SAME gRPC service the gRPC listener serves, using the
// google.api.http bindings already in the .proto. There is no second implementation of any
// handler, and there is no second copy of the business rules -- which is the entire reason to
// use grpc-gateway rather than writing REST handlers by hand.
//
// CLIENT MODE, NOT SERVER MODE. This is the decision the whole package turns on.
//
// The generated code offers RegisterOrderServiceHandlerServer, which calls the handler
// implementation DIRECTLY -- an in-process function call that skips grpc.Server entirely. It
// is faster, it needs no connection, and it is wrong here: skipping grpc.Server means
// skipping every interceptor. The REST surface would have no authentication, no
// authorisation, no admission control, no error mapping, no metrics and no tracing, while
// looking in code exactly like the gRPC surface that has all six.
//
// Measured, rather than assumed. An anonymous GET /v1/orders against a server-mode mux built
// from this repo's own handler returns 500 -- and it is 500 only because grpcapi's tenantOf
// refuses to proceed without a principal. A handler that defaulted the tenant instead, which
// is the more common shape, would have returned 200 and somebody else's orders. The failure
// mode is "depends how defensive each handler happens to be", which is not a security model.
//
// So the gateway dials a real gRPC connection and every request makes a real RPC.
// gateway_test.go::TestRESTGoesThroughTheInterceptorChain proves it by sending an
// unauthenticated HTTP request and requiring 401 -- a code only the auth interceptor produces.
package gateway

import (
	"context"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
)

// NewMux builds the REST mux over an existing gRPC connection.
//
// The connection is supplied rather than dialled here so the caller decides what it points
// at. internal/app hands it an in-process bufconn, which keeps the transcoder a zero-hop
// translation while still running the full chain.
func NewMux(ctx context.Context, conn *grpc.ClientConn) (*runtime.ServeMux, error) {
	mux := runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, jsonMarshaler()),
		runtime.WithErrorHandler(handleError),

		// Streaming errors arrive after the response has begun, so they cannot change the
		// status code. Without this they are rendered in grpc-gateway's default shape while
		// every other error in the service uses ours.
		runtime.WithStreamErrorHandler(handleStreamError),

		runtime.WithIncomingHeaderMatcher(incomingHeaders),
	)

	if err := orderv1.RegisterOrderServiceHandler(ctx, mux, conn); err != nil {
		return nil, err
	}
	return mux, nil
}

// incomingHeaders decides which HTTP headers become gRPC metadata.
//
// THE DEFAULT MATCHER DROPS traceparent, which silently severed trace context for every caller
// of the REST edge.
//
// grpc-gateway forwards only headers prefixed with `Grpc-Metadata-`, plus a fixed list of
// permanent HTTP headers. W3C Trace Context postdates that list and is not on it, so an
// instrumented HTTP client sending a perfectly good traceparent had it discarded at the edge:
// the gRPC handler began a fresh root span, the outbox row recorded THAT trace, and the
// caller's trace ended at the gateway with no error and nothing in any log.
//
// Measured rather than reasoned. An end-to-end test sent the same traceparent over both
// surfaces and read the outbox row that resulted: the gRPC path carried the caller's trace id
// through to the row, the HTTP path wrote an empty trace_parent.
//
// tracestate travels with it. It is the vendor half of the same standard, and forwarding one
// without the other discards sampling decisions that some backends encode there.
func incomingHeaders(key string) (string, bool) {
	switch textproto.CanonicalMIMEHeaderKey(key) {
	case "Traceparent", "Tracestate":
		// Lower-cased: gRPC metadata keys are lower-case, and the W3C propagator looks for
		// exactly "traceparent".
		return strings.ToLower(key), true
	default:
		// Everything else keeps grpc-gateway's own rules. Widening this further is a decision
		// about what a caller may inject into the service's own metadata, so it belongs one
		// header at a time rather than as a blanket forward.
		return runtime.DefaultHeaderMatcher(key)
	}
}

// jsonMarshaler configures JSON in both directions.
func jsonMarshaler() *runtime.JSONPb {
	return &runtime.JSONPb{
		MarshalOptions: protojson.MarshalOptions{
			// UseProtoNames emits `customer_id`, not `customerId`.
			//
			// protojson defaults to lowerCamelCase, which means the JSON field names differ
			// from the .proto, from the gRPC surface, from the database columns and from the
			// OpenAPI document generated off the same file. One name per field, everywhere,
			// is worth more than matching JavaScript convention.
			UseProtoNames: true,

			// EmitUnpopulated writes zero values instead of omitting them.
			//
			// Without it, `"status": "PENDING"` disappears the moment status is the zero
			// value, and a client cannot distinguish "the server did not set this" from "the
			// server set it to zero". Every typed client then needs optionality that the
			// schema says does not exist. The cost is larger payloads; the benefit is a
			// response shape that matches the declared message.
			EmitUnpopulated: true,
		},
		UnmarshalOptions: protojson.UnmarshalOptions{
			// DiscardUnknown stays FALSE, so an unrecognised field is a 400 rather than a
			// silent no-op. A client that misspells `customer_id` should be told, not left
			// wondering why every order has an empty customer. This is the single most
			// useful strictness in the whole edge.
			DiscardUnknown: false,
		},
	}
}

// Handler wraps the mux with the HTTP-level concerns the gateway owns.
//
// Deliberately thin. Anything that belongs to the request itself -- auth, limits, tracing --
// is an interceptor, so it applies to gRPC and REST alike. A middleware here would apply to
// REST only, and the two surfaces diverging is the failure this package exists to avoid.
func Handler(mux *runtime.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A body limit at the edge, mirroring grpc.MaxRecvMsgSize on the RPC side. Without
		// it, JSON is decoded before any gRPC limit applies, so the REST path would accept a
		// body the gRPC path rejects.
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		mux.ServeHTTP(w, r)
	})
}

// maxRequestBytes matches the default GRPC_MAX_RECV_MSG_BYTES. JSON is larger than the proto
// it decodes to, so this is a ceiling on the encoded form and therefore slightly generous --
// the authoritative limit is still the gRPC one, applied after transcoding.
const maxRequestBytes = 4 << 20
