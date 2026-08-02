// Package apperr is the one error model for the whole service.
//
// It exists because the alternative -- every layer inventing its own error type -- ends
// with a handler that cannot tell "the customer sent bad input" from "the database is
// down", so everything becomes a 500 and the on-call engineer learns nothing.
//
// There is exactly ONE table mapping Kind to a gRPC code and an HTTP status, in this
// file. mapping_test.go walks it exhaustively, so the gRPC and REST surfaces cannot drift
// apart as the service grows.
//
// Note this package is NOT imported by internal/order. The domain returns plain sentinel
// errors and the boundary translates them, which is what keeps the domain free of any
// transport dependency.
package apperr

import (
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// Kind is the class of failure. It is deliberately small: a taxonomy nobody can hold in
// their head gets used inconsistently, which is worse than a coarse one used correctly.
type Kind uint8

const (
	// KindUnknown is the zero value and maps to Internal. An error that reaches the
	// client as Unknown is a bug in the mapping, not a category.
	KindUnknown Kind = iota

	// KindInvalidArgument: the request is malformed regardless of system state.
	KindInvalidArgument

	// KindFailedPrecondition: the request is well-formed but illegal in the current
	// state (cancelling a shipped order). Distinct from InvalidArgument because
	// retrying after the state changes may succeed.
	KindFailedPrecondition

	// KindNotFound: no such resource, or not visible to this tenant.
	KindNotFound

	// KindAlreadyExists: a uniqueness constraint rejected the write.
	KindAlreadyExists

	// KindPermissionDenied: authenticated, but not allowed.
	KindPermissionDenied

	// KindUnauthenticated: no valid credentials.
	KindUnauthenticated

	// KindResourceExhausted: rate limited or shed by admission control.
	KindResourceExhausted

	// KindUnavailable: a dependency is down. Retryable.
	KindUnavailable

	// KindInternal: a bug. Never carries detail to the client.
	KindInternal
)

// kindInfo is THE table. Adding a Kind without adding a row here fails
// TestEveryKindIsMapped, so the two can never drift.
var kindInfo = map[Kind]struct {
	Code   codes.Code
	Status int
	Name   string
}{
	KindUnknown:            {codes.Internal, http.StatusInternalServerError, "UNKNOWN"},
	KindInvalidArgument:    {codes.InvalidArgument, http.StatusBadRequest, "INVALID_ARGUMENT"},
	KindFailedPrecondition: {codes.FailedPrecondition, http.StatusBadRequest, "FAILED_PRECONDITION"},
	KindNotFound:           {codes.NotFound, http.StatusNotFound, "NOT_FOUND"},
	KindAlreadyExists:      {codes.AlreadyExists, http.StatusConflict, "ALREADY_EXISTS"},
	KindPermissionDenied:   {codes.PermissionDenied, http.StatusForbidden, "PERMISSION_DENIED"},
	KindUnauthenticated:    {codes.Unauthenticated, http.StatusUnauthorized, "UNAUTHENTICATED"},
	KindResourceExhausted:  {codes.ResourceExhausted, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED"},
	KindUnavailable:        {codes.Unavailable, http.StatusServiceUnavailable, "UNAVAILABLE"},
	KindInternal:           {codes.Internal, http.StatusInternalServerError, "INTERNAL"},
}

// HTTPStatusFromCode maps a gRPC code to an HTTP status using THE SAME TABLE.
//
// The REST edge needs this direction: by the time an error reaches the gateway it is a gRPC
// status, and the Kind that produced it is gone. Deriving the answer from kindInfo rather
// than writing a second switch is what keeps the two surfaces honest -- grpc-gateway ships
// its own runtime.HTTPStatusFromCode, and using it would mean this service's REST status
// codes were decided by a dependency's table while its gRPC codes came from ours. They agree
// today; nothing would tell us when they stopped.
//
// Several Kinds share a code (KindUnknown and KindInternal are both Internal), which is fine:
// they also share an HTTP status, and mapping_test.go asserts that no code maps to two
// different statuses.
func HTTPStatusFromCode(code codes.Code) int {
	for _, info := range kindInfo {
		if info.Code == code {
			return info.Status
		}
	}
	// codes.OK and codes.Canceled/DeadlineExceeded have no Kind. 500 is the safe default:
	// an unmapped code means an error path nobody classified.
	switch code {
	case codes.OK:
		return http.StatusOK
	case codes.Canceled:
		// 499, nginx's "client closed request". Not in net/http, and worth distinguishing
		// from a server fault: a cancelled request is the CALLER hanging up, and counting it
		// as a 5xx makes your own error-rate alert fire every time someone closes a tab.
		return 499
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.Unimplemented:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

// AllKinds is every Kind, so tests can iterate exhaustively rather than listing by hand
// and quietly missing the one added last week.
func AllKinds() []Kind {
	return []Kind{
		KindUnknown, KindInvalidArgument, KindFailedPrecondition, KindNotFound,
		KindAlreadyExists, KindPermissionDenied, KindUnauthenticated,
		KindResourceExhausted, KindUnavailable, KindInternal,
	}
}

// GRPCCode is the gRPC status code for this Kind.
func (k Kind) GRPCCode() codes.Code {
	if info, ok := kindInfo[k]; ok {
		return info.Code
	}
	return codes.Internal
}

// HTTPStatus is the HTTP status the gateway returns for this Kind. It matches
// grpc-gateway's default code mapping so the two surfaces agree.
func (k Kind) HTTPStatus() int {
	if info, ok := kindInfo[k]; ok {
		return info.Status
	}
	return http.StatusInternalServerError
}

func (k Kind) String() string {
	if info, ok := kindInfo[k]; ok {
		return info.Name
	}
	return "UNKNOWN"
}

// Redacts reports whether errors of this Kind must have their message replaced before
// reaching a client. Internal failures carry driver text, table names, and occasionally
// row data; none of that is the caller's business.
func (k Kind) Redacts() bool {
	return k == KindInternal || k == KindUnknown
}

// Error is an application error carrying enough structure for a machine-readable
// response.
type Error struct {
	Kind Kind

	// Reason is a stable, machine-readable code in SCREAMING_SNAKE_CASE, e.g.
	// "ORDER_NOT_FOUND". This -- not the message -- is what clients branch on. It becomes
	// ErrorInfo.reason in the gRPC status details.
	Reason string

	// Message is operator-facing English for logs and debugging. It is explicitly NOT a
	// contract and NOT localized: clients render user-facing text from Reason. Saying so
	// out loud is what stops someone building a UI that string-matches on it.
	Message string

	// Metadata becomes ErrorInfo.metadata. Keep it small and never put PII in it.
	Metadata map[string]string

	// Details are extra google.rpc detail messages attached to the status, e.g. a
	// BadRequest listing field violations or a RetryInfo telling the caller when to come
	// back. They are the machine-readable half of the error: the message is for humans,
	// these are for code.
	Details []proto.Message

	wrapped error
}

// WithDetails attaches google.rpc detail messages.
func (e *Error) WithDetails(details ...proto.Message) *Error {
	e.Details = append(e.Details, details...)
	return e
}

// New builds an Error.
func New(kind Kind, reason, message string) *Error {
	return &Error{Kind: kind, Reason: reason, Message: message}
}

// Wrap attaches a Kind and reason to an existing error, preserving it for errors.Is/As
// and for the log. The wrapped error is never shown to the client.
func Wrap(err error, kind Kind, reason, message string) *Error {
	return &Error{Kind: kind, Reason: reason, Message: message, wrapped: err}
}

// WithMetadata attaches structured key/values.
func (e *Error) WithMetadata(kv map[string]string) *Error {
	if e.Metadata == nil {
		e.Metadata = make(map[string]string, len(kv))
	}
	for k, v := range kv {
		e.Metadata[k] = v
	}
	return e
}

func (e *Error) Error() string {
	if e.wrapped != nil {
		return fmt.Sprintf("%s: %s: %v", e.Reason, e.Message, e.wrapped)
	}
	return fmt.Sprintf("%s: %s", e.Reason, e.Message)
}

// Unwrap keeps errors.Is and errors.As working through the boundary, so a handler can
// still ask "was this ultimately order.ErrNotFound?".
func (e *Error) Unwrap() error { return e.wrapped }

// From extracts an *Error from anywhere in the chain.
func From(err error) (*Error, bool) {
	var ae *Error
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// KindOf reports the Kind of any error. An error that was never classified is
// KindInternal, not KindUnknown-and-therefore-fine: unclassified means a code path
// nobody thought about, and defaulting it to a 500 is the safe direction.
func KindOf(err error) Kind {
	if err == nil {
		return KindUnknown
	}
	if ae, ok := From(err); ok {
		return ae.Kind
	}
	return KindInternal
}

// ClientMessage is the text safe to return to a caller.
//
// For internal failures this is the literal string "internal error" -- never the wrapped
// error. Leaking `pq: duplicate key value violates unique constraint "users_email_key"`
// tells an attacker your table names, your column names, and that the address they tried
// is already registered. mapping_test.go asserts both halves: the driver text is absent
// from the response AND present in the log.
func (e *Error) ClientMessage() string {
	if e.Kind.Redacts() {
		return "internal error"
	}
	return e.Message
}
