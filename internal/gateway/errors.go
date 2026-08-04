package gateway

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/example/gomicro/internal/platform/apperr"
)

// errorBody is the ONE shape every REST error takes.
//
// Modelled on AIP-193 rather than invented: an `error` object holding a symbolic status, the
// stable machine-readable reason, a human message, and structured details. Clients branch on
// `reason`; `message` is explicitly not a contract, exactly as on the gRPC side.
//
// grpc-gateway's default body is `{"code": 5, "message": "..."}` -- a NUMERIC gRPC code,
// which is meaningless to an HTTP client that never sees gRPC, and no reason field at all. A
// REST consumer would be left string-matching on the message, which is the thing every error
// model in this repo is designed to prevent.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	// Code is the SYMBOLIC gRPC code, e.g. "NotFound". A symbol survives being read in a
	// log; the integer 5 does not.
	//
	// The spelling is grpc-go's codes.Code.String(), which is CamelCase -- "NotFound",
	// "ResourceExhausted", "Unauthenticated". This comment used to give the example as
	// "NOT_FOUND", which is the SCREAMING_SNAKE_CASE of google.rpc.Code and of apperr's
	// Reason, and is not what this field ever contained. A client author who took the
	// example literally would branch on a value that never arrives -- and would find out
	// from a bug report, since a non-matching branch fails silently.
	Code string `json:"code"`

	// Reason is the stable identifier clients branch on, e.g. "ORDER_NOT_FOUND". It comes
	// from the ErrorInfo detail every apperr attaches.
	Reason string `json:"reason,omitempty"`

	Message string `json:"message"`

	// Details carries the google.rpc detail messages -- BadRequest field violations,
	// RetryInfo -- as JSON, so a REST client gets the same machine-readable payload a gRPC
	// client does. Dropping them here is the usual shortcut and it makes the REST surface
	// strictly less useful than the gRPC one for exactly the errors clients must handle.
	Details []json.RawMessage `json:"details,omitempty"`

	// Domain identifies the emitting service, so a client aggregating several APIs can tell
	// whose ORDER_NOT_FOUND it is looking at.
	Domain string `json:"domain,omitempty"`
}

// handleError renders any gRPC error as JSON.
//
// It replaces runtime.DefaultHTTPErrorHandler wholesale. The HTTP status comes from apperr's
// table -- the same table that produced the gRPC code -- rather than from grpc-gateway's own
// mapping, so the two surfaces cannot disagree about what a NotFound is.
func handleError(
	ctx context.Context,
	mux *runtime.ServeMux,
	marshaler runtime.Marshaler,
	w http.ResponseWriter,
	r *http.Request,
	err error,
) {
	st := status.Convert(err)

	// Client hang-ups are not server faults. status.Convert turns a cancelled context into
	// codes.Canceled; rendering that as a 500 makes every closed browser tab an error-budget
	// event.
	httpStatus := apperr.HTTPStatusFromCode(st.Code())

	body := errorBody{Error: errorDetail{
		Code:    st.Code().String(),
		Message: st.Message(),
	}}

	for _, detail := range st.Details() {
		msg, ok := detail.(proto.Message)
		if !ok {
			continue
		}
		// ErrorInfo is lifted out of details into first-class fields, because reason and
		// domain are what clients actually use and burying them in an array makes every
		// consumer write a search.
		if info, isInfo := msg.(*errdetails.ErrorInfo); isInfo {
			body.Error.Reason = info.GetReason()
			body.Error.Domain = info.GetDomain()
			continue
		}
		if raw, mErr := protojson.Marshal(msg); mErr == nil {
			body.Error.Details = append(body.Error.Details, raw)
		}
	}

	w.Header().Set("Content-Type", "application/json")

	// Forward any metadata the handler set, so a Retry-After style header survives. Done
	// BEFORE WriteHeader, which is the only point at which headers can still be set.
	writeGRPCMetadata(ctx, w)

	w.WriteHeader(httpStatus)
	if encErr := json.NewEncoder(w).Encode(body); encErr != nil {
		// The status line is already written, so there is nothing useful left to say to the
		// client. Falling back to a plain string keeps the connection sane.
		http.Error(w, `{"error":{"code":"INTERNAL","message":"internal error"}}`, http.StatusInternalServerError)
	}
}

// THERE IS DELIBERATELY NO handleStreamError HERE, and the absence is the honest answer
// rather than an oversight.
//
// One was registered via runtime.WithStreamErrorHandler, under a comment saying that without
// it streaming errors "are rendered in grpc-gateway's default shape while every other error
// in the service uses ours". Its body was:
//
//	st := status.Convert(err)
//	if st.Code() == codes.OK { return st }
//	return st
//
// Both branches return the same value, and runtime.DefaultStreamErrorHandler is literally
// `return status.Convert(err)`. It was the default, re-implemented, described as replacing
// the default.
//
// The comment could not have been satisfied by writing more code, which is the part worth
// keeping. StreamErrorHandlerFunc returns a *status.Status, and the runtime marshals it as
// `{"error": <google.rpc.Status>}` -- so the extension point chooses WHICH status is
// rendered, never the JSON shape. errorBody's symbolic "code": "NOT_FOUND" and top-level
// "reason" are unreachable from here at any level of effort.
//
// So a mid-stream error over REST looks like this, and it differs from every unary error:
//
//	{"error":{"code":8,"message":"request quota exceeded","details":[...]}}
//
// The code is NUMERIC and the reason lives inside details as an ErrorInfo. Two further
// consequences, neither obvious:
//
//   - A stream that fails BEFORE its first message has not written a header yet, so the HTTP
//     status comes from grpc-gateway's own HTTPStatusFromCode -- not apperr's table. They
//     agree today for every Kind this service emits; nothing enforces that they keep agreeing.
//   - A stream that fails AFTER its first message has already sent 200, so the failure is a
//     trailing chunk in a successful response. A REST client that checks only the status code
//     will read a truncated result as complete.
//
// A fork that needs one JSON error shape across both surfaces cannot get it from this seam;
// it needs its own streaming handler wrapping the mux.

// writeGRPCMetadata copies server metadata onto the HTTP response.
//
// grpc-gateway's default handler does this and a custom one that forgets it silently drops
// anything a handler set via grpc.SetHeader -- which is how the rate limiter's Retry-After
// reaches a REST client at all.
//
// THE PREFIX IS DROPPED FOR STANDARD HEADERS, and keeping it made the forwarding pointless.
//
// Every value used to go out as runtime.MetadataHeaderPrefix+key, i.e.
// `Grpc-Metadata-Retry-After`. Retry-After is an RFC 9110 header that HTTP clients, proxies
// and browsers already act on; under that prefix it is an unrecognised string that nothing
// honours. The rate limiter set it, the gateway forwarded it, a test could see it arriving,
// and no client would ever have backed off -- which is the whole reason it is sent.
//
// x-request-id keeps the prefix. It is not a standard header, so there is no behaviour to
// preserve, and the prefix marks it as having come from the gRPC side.
func writeGRPCMetadata(ctx context.Context, w http.ResponseWriter) {
	md, ok := runtime.ServerMetadataFromContext(ctx)
	if !ok {
		return
	}
	for key, values := range md.HeaderMD {
		// Only forward what was explicitly marked for the wire. Copying all metadata would
		// leak internal headers -- and anything an interceptor attached for its own use --
		// into an HTTP response.
		if !isForwardable(key) {
			continue
		}
		name := runtime.MetadataHeaderPrefix + key
		if isStandardHTTPHeader(key) {
			name = key
		}
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
}

// isForwardable is the allowlist for metadata reaching an HTTP client.
//
// An allowlist rather than a denylist: the failure mode of getting a denylist wrong is a
// leaked internal header, and you find out from whoever reads it.
func isForwardable(key string) bool {
	switch key {
	case "retry-after", "x-request-id":
		return true
	default:
		return false
	}
}

// isStandardHTTPHeader reports whether a key must reach the client under its real name to
// mean anything.
//
// Deliberately a separate, smaller list than isForwardable. Forwarding decides what a client
// is ALLOWED to see; this decides what a client can ACT on. Conflating them would mean every
// future addition to the allowlist silently claimed standard-header semantics.
func isStandardHTTPHeader(key string) bool {
	return key == "retry-after"
}
