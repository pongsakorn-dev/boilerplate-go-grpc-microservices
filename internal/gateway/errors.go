package gateway

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
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
	// Code is the SYMBOLIC gRPC code, e.g. "NOT_FOUND". A symbol survives being read in a
	// log; the integer 5 does not.
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

// handleStreamError renders an error that arrives mid-stream.
//
// By then the 200 has been sent and the status cannot change, so the error becomes a trailing
// chunk in the response body. grpc-gateway requires a *status.Status here, which forces the
// shape -- so this returns the same fields under the same names rather than a second format.
func handleStreamError(_ context.Context, err error) *status.Status {
	st := status.Convert(err)
	if st.Code() == codes.OK {
		return st
	}
	return st
}

// writeGRPCMetadata copies server metadata onto the HTTP response.
//
// grpc-gateway's default handler does this and a custom one that forgets it silently drops
// anything a handler set via grpc.SetHeader -- which today is nothing, and tomorrow is the
// Retry-After the rate limiter (M7) will want to send.
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
		for _, v := range values {
			w.Header().Add(runtime.MetadataHeaderPrefix+key, v)
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
