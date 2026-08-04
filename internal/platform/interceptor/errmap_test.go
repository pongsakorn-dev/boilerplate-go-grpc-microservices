package interceptor

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/example/gomicro/internal/platform/apperr"
)

// A handler that returns ctx.Err() is not an exotic case -- it is what every correctly
// written handler does when its caller goes away, and the pattern the standard library
// teaches. It has to map to something honest.
//
// It did not. context.Canceled implements no GRPCStatus(), so status.FromError declined it,
// mapErr fell through to apperr.ToError, and the unclassified branch stamped it
// codes.Internal. Two things went wrong at once, and the second is the expensive one:
//
//	the client, which had just hung up, was told the SERVER had failed; and
//	every routine cancellation was counted in the Internal error-rate series.
//
// The second is what makes this worth a test rather than a shrug. The Internal rate is the
// series you page on. Give it a permanent floor made of clients closing tabs and it stops
// meaning anything, so the alert either gets a threshold high enough to miss real faults or
// gets muted -- and either way the next genuine incident arrives unannounced.
//
// The give-away that this was always a mistake rather than a decision: apperr's
// HTTPStatusFromCode already had a careful arm mapping codes.Canceled to 499, with a comment
// explaining that counting a hang-up as a 5xx breaks your own alerting. That arm could never
// run, because nothing in the service could produce codes.Canceled in the first place.
func TestContextErrorsAreNotOurFault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		handlerErr error
		wantCode   codes.Code
		wantReason string
	}{
		{
			name:       "the caller cancelled",
			handlerErr: context.Canceled,
			wantCode:   codes.Canceled,
			wantReason: "CANCELED",
		},
		{
			name:       "the caller's deadline elapsed",
			handlerErr: context.DeadlineExceeded,
			wantCode:   codes.DeadlineExceeded,
			wantReason: "DEADLINE_EXCEEDED",
		},
		{
			// Handlers almost never return the bare sentinel. They return something that
			// wrapped it on the way up through three layers of repository code, so
			// classification has to walk the chain rather than compare with ==.
			name:       "a cancellation wrapped on the way up",
			handlerErr: fmt.Errorf("load order items: %w", context.Canceled),
			wantCode:   codes.Canceled,
			wantReason: "CANCELED",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := callThrough(t, tc.handlerErr)

			st, ok := status.FromError(err)
			if !ok {
				t.Fatalf("ErrorMap returned a non-status error: %v", err)
			}
			if st.Code() != tc.wantCode {
				t.Errorf("code = %s, want %s.\n\n"+
					"A context error is the CALLER's doing. Reporting it as %s tells them "+
					"this service broke, and puts routine disconnects into the error-rate "+
					"series that on-call pages on.",
					st.Code(), tc.wantCode, st.Code())
			}
			if got := reasonOf(t, st); got != tc.wantReason {
				t.Errorf("ErrorInfo.reason = %q, want %q", got, tc.wantReason)
			}
		})
	}
}

// TestUnclassifiedErrorsStillRedact is the other half, and it exists because the fix above
// is one careless `default:` away from classifying everything as somebody else's problem.
//
// Adding Kinds for context errors must not soften the rule for errors nobody classified:
// those still become Internal, and their text still never reaches the caller.
func TestUnclassifiedErrorsStillRedact(t *testing.T) {
	t.Parallel()

	const driverText = `pq: duplicate key value violates unique constraint "orders_pkey"`

	_, err := callThrough(t, errors.New(driverText))

	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("code = %s, want Internal -- an unclassified error is a path nobody "+
			"thought about, and 500 is the safe direction", st.Code())
	}
	if st.Message() == driverText {
		t.Error("the driver text reached the client verbatim, leaking the schema")
	}
	if st.Message() != "internal error" {
		t.Errorf("message = %q, want the literal %q", st.Message(), "internal error")
	}
}

// TestADeliberateStatusIsLeftAlone guards the branch above the classifier: a handler that
// went to the trouble of producing a status meant it, and mapErr must not overwrite it.
func TestADeliberateStatusIsLeftAlone(t *testing.T) {
	t.Parallel()

	_, err := callThrough(t, status.Error(codes.Unimplemented, "not built yet"))

	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %s, want Unimplemented", st.Code())
	}
}

// callThrough runs the real ErrorMap interceptor over a handler that returns handlerErr.
func callThrough(t *testing.T, handlerErr error) (any, error) {
	t.Helper()

	return ErrorMap("orderd")(
		context.Background(),
		nil,
		&grpc.UnaryServerInfo{FullMethod: "/order.v1.OrderService/GetOrder"},
		func(context.Context, any) (any, error) { return nil, handlerErr },
	)
}

// reasonOf digs the stable machine-readable token out of the status details.
//
// The reason matters as much as the code here: a client branching on CANCELED needs it to be
// spelled the same way every time, and it is what makes the two new Kinds visible in a log
// line rather than just a numeric code.
func reasonOf(t *testing.T, st *status.Status) string {
	t.Helper()

	for _, d := range st.Details() {
		if info, ok := d.(interface{ GetReason() string }); ok {
			return info.GetReason()
		}
	}
	t.Fatalf("status carried no ErrorInfo: %v", st.Details())
	return ""
}

// Compile-time reminder that the Kinds these tests depend on are the ones in THE table.
var _ = []apperr.Kind{apperr.KindCanceled, apperr.KindDeadlineExceeded}
