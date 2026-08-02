package client_test

import (
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/example/gomicro/internal/platform/apperr"
	"github.com/example/gomicro/internal/platform/client"
)

// TestUpstreamCodesMapToWhatIsTrueAboutYourService pins the reverse mapping.
//
// The table is short because the DECISION is short: your caller can act on "the upstream is
// having trouble, back off" and cannot act on "the upstream disliked an argument you never
// sent them". Anything in the second category is your bug, and Internal is its honest name.
func TestUpstreamCodesMapToWhatIsTrueAboutYourService(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code codes.Code
		want apperr.Kind
	}{
		// The upstream is struggling. Retryable, and a caller can do something about it.
		{codes.Unavailable, apperr.KindUnavailable},
		{codes.DeadlineExceeded, apperr.KindUnavailable},
		{codes.ResourceExhausted, apperr.KindUnavailable},
		{codes.Canceled, apperr.KindUnavailable},

		// The upstream answered, and its answer is about a request your caller never made.
		{codes.NotFound, apperr.KindInternal},
		{codes.InvalidArgument, apperr.KindInternal},
		{codes.PermissionDenied, apperr.KindInternal},
		{codes.Unauthenticated, apperr.KindInternal},
		{codes.AlreadyExists, apperr.KindInternal},
		{codes.Unimplemented, apperr.KindInternal},
		{codes.Internal, apperr.KindInternal},
	}

	for _, tc := range cases {
		t.Run(tc.code.String(), func(t *testing.T) {
			t.Parallel()

			e := &client.Error{Target: "inventory:50051", Method: "/x/Y", Code: tc.code}
			if got := e.Kind(); got != tc.want {
				t.Errorf("Kind() for %s = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}

// TestAnUnauthenticatedUpstreamIsNotAnUnauthenticatedCaller is the mapping worth stating on its
// own, because passing it through is a genuine security-shaped bug.
//
// If this service's credential is wrong, the upstream answers Unauthenticated. Forwarding that
// tells your caller to go and re-authenticate -- so they discard a perfectly good session, log
// in again, and get the same answer, while the actual fault is a misconfigured service
// credential that nobody is looking at.
func TestAnUnauthenticatedUpstreamIsNotAnUnauthenticatedCaller(t *testing.T) {
	t.Parallel()

	e := &client.Error{Target: "inventory:50051", Method: "/x/Y", Code: codes.Unauthenticated}

	if got := e.Kind(); got == apperr.KindUnauthenticated {
		t.Error("an upstream's Unauthenticated became the caller's.\n\n" +
			"The caller's credential is fine; THIS SERVICE's is not. Telling them to log in " +
			"again sends them round a loop that cannot terminate.")
	}
}

// TestTheDefaultConversionNamesTheUpstreamInTheLogButNotToTheCaller covers both halves of
// redaction at once.
func TestTheDefaultConversionNamesTheUpstreamInTheLogButNotToTheCaller(t *testing.T) {
	t.Parallel()

	e := &client.Error{Target: "inventory:50051", Method: "/inv.v1.Inventory/Get", Code: codes.Internal}
	app := e.AsAppError()

	if got := app.Metadata["upstream"]; got != "inventory:50051" {
		t.Errorf("metadata[upstream] = %q, want the dependency named for the operator", got)
	}

	// KindInternal redacts on the way out, so the detail above stays in the logs.
	if msg := app.ClientMessage(); msg != "internal error" {
		t.Errorf("ClientMessage() = %q; an internal fault must not describe your topology to "+
			"the caller", msg)
	}
	if app.Message == "internal error" {
		t.Error("the internal message was redacted at construction, so the operator loses the " +
			"one detail worth having")
	}
}
