package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/example/gomicro/internal/platform/apperr"
)

// Error is an upstream failure, kept distinct from your own.
//
// THE CALLEE'S STATUS IS NOT YOUR STATUS. This is the mistake this type exists to make hard.
//
// Returning the upstream's error unchanged looks like the helpful thing to do -- the code and
// message are right there, already shaped like a gRPC error. What actually happens is that
// this service's ErrorMap interceptor sees a valid *status.Error that is not an *apperr.Error
// and forwards it verbatim, so your caller receives the UPSTREAM's code, the UPSTREAM's
// message, and an ErrorInfo naming the UPSTREAM's service as the domain.
//
// Concretely: inventory answers NotFound because SKU-9 does not exist, you pass it through,
// and your caller concludes the ORDER does not exist. They retry against a different order id
// and get the same answer forever. The bug is unfalsifiable from the outside, because the
// error is perfectly well-formed and describes something true about a service your caller has
// never heard of.
//
// So an upstream failure arrives here as an Error, which is not a *status.Error and cannot be
// returned to a caller by accident. Turning it into something your callers should see is a
// deliberate act: Kind for the safe default, or AppError to say exactly what it means in your
// domain.
type Error struct {
	// Target is the upstream this client was built for -- the address or logical name.
	Target string

	// Method is the full "/package.Service/Method" that failed.
	Method string

	// Code is the gRPC code the callee returned.
	Code codes.Code

	// Reason and Domain come from the callee's google.rpc.ErrorInfo, when it sent one.
	//
	// Reason is the stable, machine-readable token ("ORDER_NOT_FOUND"); Domain is the
	// callee's own service name. Both are empty when the upstream is not built from this
	// template, or when the failure happened before any handler ran -- so neither may be
	// assumed present.
	Reason string
	Domain string

	// Message is the callee's client-facing message. For an upstream built from this
	// template, an internal fault has already been redacted to "internal error" -- the
	// original text stayed in the callee's logs, which is where it belongs.
	Message string

	// RetryAfter is the callee's google.rpc.RetryInfo, if it sent one. The rate limiter and
	// the admission limiter both do.
	RetryAfter time.Duration

	err error
}

// Error implements error.
func (e *Error) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("upstream %s: %s failed: %s (%s)", e.Target, e.Method, e.Code, e.Reason)
	}
	return fmt.Sprintf("upstream %s: %s failed: %s", e.Target, e.Method, e.Code)
}

// Unwrap exposes the original status error, so errors.Is and status.FromError still work for
// callers that genuinely need the wire detail.
func (e *Error) Unwrap() error { return e.err }

// From extracts an upstream Error from an error chain.
func From(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// Kind is the DEFAULT way an upstream failure should surface in your own domain.
//
// It maps to what is true about YOUR service, which is almost never what the callee said:
//
//	the upstream was unreachable, slow, or throttling  ->  KindUnavailable
//	everything else                                    ->  KindInternal
//
// A caller of yours can act on Unavailable -- back off and retry. They cannot act on
// "inventory said InvalidArgument", because they did not send that argument: you did. That is
// your bug, and KindInternal is the honest name for it.
//
// The deliberate exception is a translation you write yourself with AppError, when a callee's
// answer really does mean something in your domain -- "inventory says this SKU does not
// exist" genuinely is an InvalidArgument on the order the caller submitted.
func (e *Error) Kind() apperr.Kind {
	switch e.Code {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.ResourceExhausted:
		return apperr.KindUnavailable
	default:
		return apperr.KindInternal
	}
}

// AppError translates an upstream failure into one of your own, deliberately.
//
// The upstream error is kept as the cause, so it stays in the logs and in errors.Is chains
// while your caller sees only what you chose to say. The callee's details are NOT copied
// forward: a BadRequest describing the callee's field names would be actively misleading
// attached to a response about your API.
func (e *Error) AppError(kind apperr.Kind, reason, message string) *apperr.Error {
	return apperr.Wrap(e, kind, reason, message)
}

// AsAppError is the safe default conversion, for a handler that has no better answer.
//
// It applies Kind and attaches the upstream, method and code as error metadata so the log line
// names the dependency. Use it when the upstream's failure has no specific meaning in your
// domain; use AppError when it does.
func (e *Error) AsAppError() *apperr.Error {
	kind := e.Kind()

	reason := "UPSTREAM_UNAVAILABLE"
	message := "an upstream service is unavailable"
	if kind == apperr.KindInternal {
		reason = "UPSTREAM_FAILED"
		// KindInternal redacts the message on the way out, so this text reaches the log and
		// not the caller. Saying which upstream and which method is the whole value of it.
		message = fmt.Sprintf("upstream %s returned %s from %s", e.Target, e.Code, e.Method)
	}

	return apperr.Wrap(e, kind, reason, message).WithMetadata(map[string]string{
		"upstream":        e.Target,
		"upstream_method": e.Method,
		"upstream_code":   e.Code.String(),
	})
}

// newUpstreamError converts a gRPC status from an upstream into an Error.
func newUpstreamError(target, method string, err error) error {
	if err == nil {
		return nil
	}

	st, ok := status.FromError(err)
	if !ok {
		// Not a status at all -- a local failure inside an interceptor below us, or a
		// context error. Wrapping it as an upstream status would invent a wire response
		// that never happened.
		return err
	}

	e := &Error{
		Target:  target,
		Method:  method,
		Code:    st.Code(),
		Message: st.Message(),
		err:     err,
	}

	for _, detail := range st.Details() {
		// Details() yields an `error` rather than a message when the Any's type URL is not
		// registered in this binary, so the proto.Message assertion is not optional.
		msg, ok := detail.(proto.Message)
		if !ok {
			continue
		}
		switch d := msg.(type) {
		case *errdetails.ErrorInfo:
			// FIRST wins. A well-formed status from this template carries exactly one
			// ErrorInfo, written by ToStatus as detail zero; if an upstream somehow sends
			// more, the first is the one its own error model chose.
			if e.Reason == "" {
				e.Reason = d.GetReason()
				e.Domain = d.GetDomain()
			}
		case *errdetails.RetryInfo:
			if delay := d.GetRetryDelay(); delay != nil {
				e.RetryAfter = delay.AsDuration()
			}
		}
	}

	return e
}

// errBudgetExhausted is returned when there was not enough time left to make the call.
var errBudgetExhausted = errors.New("client: deadline budget exhausted before the call was made")

// newBudgetError explains a call that was never attempted.
//
// It is NOT DeadlineExceeded from the upstream, and saying so precisely is the point: nothing
// was sent, nothing was retried, and the upstream is not at fault. A metric that counts this
// as an upstream timeout blames the wrong service during an incident.
func newBudgetError(target, method string, ctx context.Context) error {
	remaining := time.Duration(0)
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}

	return &Error{
		Target:  target,
		Method:  method,
		Code:    codes.DeadlineExceeded,
		Reason:  "BUDGET_EXHAUSTED",
		Message: fmt.Sprintf("not enough deadline left to call %s (%s remaining)", target, remaining),
		err:     errBudgetExhausted,
	}
}
