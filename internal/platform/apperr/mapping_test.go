package apperr

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestEveryKindIsMapped is what keeps the table honest.
//
// Adding a Kind is a one-line change; adding the corresponding row is a second line that is
// easy to forget. Without this test, the miss is silent: the new Kind falls through to the
// default and every error of that class reports Internal/500 to clients, which reads like a
// server bug rather than the mapping gap it is.
func TestEveryKindIsMapped(t *testing.T) {
	t.Parallel()

	kinds := AllKinds()
	if len(kinds) == 0 {
		t.Fatal("AllKinds is empty -- this guard would silently pass forever")
	}

	for _, k := range kinds {
		if _, ok := kindInfo[k]; !ok {
			t.Errorf("Kind %d has no row in kindInfo", k)
		}
	}

	// And the reverse: a row for a Kind that AllKinds forgot means exhaustive tests
	// elsewhere silently skip it.
	if len(kindInfo) != len(kinds) {
		t.Errorf("kindInfo has %d rows but AllKinds returns %d -- they must agree, "+
			"or every exhaustive test over AllKinds has a blind spot", len(kindInfo), len(kinds))
	}
}

// TestKindMapping pins the gRPC code AND the HTTP status together, in one table.
//
// Pinning them together is the point. The service exposes both a gRPC and a JSON/REST
// surface, and the two must never disagree about what a given failure means -- a client
// that sees 404 over HTTP and Internal over gRPC for the same condition cannot write
// correct error handling.
//
// The HTTP column matches grpc-gateway's default code mapping, so the gateway needs no
// custom translation table that could drift from this one.
func TestKindMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind   Kind
		code   codes.Code
		status int
	}{
		{KindUnknown, codes.Internal, http.StatusInternalServerError},
		{KindInvalidArgument, codes.InvalidArgument, http.StatusBadRequest},
		{KindFailedPrecondition, codes.FailedPrecondition, http.StatusBadRequest},
		{KindNotFound, codes.NotFound, http.StatusNotFound},
		{KindAlreadyExists, codes.AlreadyExists, http.StatusConflict},
		{KindPermissionDenied, codes.PermissionDenied, http.StatusForbidden},
		{KindUnauthenticated, codes.Unauthenticated, http.StatusUnauthorized},
		{KindResourceExhausted, codes.ResourceExhausted, http.StatusTooManyRequests},
		{KindUnavailable, codes.Unavailable, http.StatusServiceUnavailable},
		{KindInternal, codes.Internal, http.StatusInternalServerError},
	}

	if len(tests) != len(AllKinds()) {
		t.Fatalf("this table has %d rows but there are %d Kinds -- add the missing row",
			len(tests), len(AllKinds()))
	}

	for _, tc := range tests {
		t.Run(tc.kind.String(), func(t *testing.T) {
			t.Parallel()
			if got := tc.kind.GRPCCode(); got != tc.code {
				t.Errorf("GRPCCode() = %v, want %v", got, tc.code)
			}
			if got := tc.kind.HTTPStatus(); got != tc.status {
				t.Errorf("HTTPStatus() = %d, want %d", got, tc.status)
			}
		})
	}
}

// TestInternalErrorsNeverLeakAndNeverLose asserts BOTH halves of the redaction rule in one
// test, because getting either half alone is a real failure.
//
// Leak: returning `pq: duplicate key value violates unique constraint "users_email_key"`
// tells an attacker your table names, your column names, and confirms the address they
// tried is already registered.
//
// Lose: redacting it out of the LOG as well leaves the on-call engineer with "internal
// error" and nothing else, which is how a five-minute fix becomes an hour of guessing.
func TestInternalErrorsNeverLeakAndNeverLose(t *testing.T) {
	t.Parallel()

	const driverText = `pq: duplicate key value violates unique constraint "users_email_key"`
	cause := errors.New(driverText)

	appErr := Wrap(cause, KindInternal, "DB_WRITE_FAILED", "could not persist the order")

	// Half one: the client sees nothing useful.
	if got := appErr.ClientMessage(); got != "internal error" {
		t.Errorf("ClientMessage() = %q, want exactly %q", got, "internal error")
	}

	st := ToStatus(appErr, "orders.example.com")
	if strings.Contains(st.Message(), "users_email_key") {
		t.Errorf("the status message leaks the driver text: %q", st.Message())
	}
	if strings.Contains(st.Message(), "duplicate key") {
		t.Errorf("the status message leaks the driver text: %q", st.Message())
	}

	// Half two: the full cause is still reachable for the log.
	if !strings.Contains(appErr.Error(), driverText) {
		t.Errorf("the driver text was lost from the error chain: %q", appErr.Error())
	}
	if !errors.Is(appErr, cause) {
		t.Error("errors.Is can no longer reach the cause; the log and any retry logic lose it")
	}
}

// TestToStatusCarriesErrorInfo proves clients get something machine-readable.
//
// The message is explicitly not a contract -- it is English operator text and may change.
// ErrorInfo.reason is the stable identifier clients branch on, so it has to actually be
// present on the wire rather than merely set on the struct.
func TestToStatusCarriesErrorInfo(t *testing.T) {
	t.Parallel()

	appErr := New(KindNotFound, "ORDER_NOT_FOUND", "no such order").
		WithMetadata(map[string]string{"order_id": "abc"})

	st := ToStatus(appErr, "orders.example.com")

	if st.Code() != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", st.Code())
	}

	var info *errdetails.ErrorInfo
	for _, d := range st.Details() {
		if ei, ok := d.(*errdetails.ErrorInfo); ok {
			info = ei
		}
	}
	if info == nil {
		t.Fatal("status carries no ErrorInfo detail; clients have nothing stable to branch on")
	}
	if info.GetReason() != "ORDER_NOT_FOUND" {
		t.Errorf("reason = %q, want ORDER_NOT_FOUND", info.GetReason())
	}
	if info.GetDomain() != "orders.example.com" {
		t.Errorf("domain = %q, want orders.example.com", info.GetDomain())
	}
	if info.GetMetadata()["order_id"] != "abc" {
		t.Errorf("metadata = %v, want order_id=abc", info.GetMetadata())
	}
}

// TestToStatusOnUnclassifiedError proves the default is safe.
//
// An error nobody classified means a code path nobody thought about. Defaulting it to
// Internal with a redacted message is the safe direction; defaulting it to something
// friendlier would hand the caller whatever text happened to be in the error.
func TestToStatusOnUnclassifiedError(t *testing.T) {
	t.Parallel()

	st := ToStatus(errors.New("connection refused to 10.0.1.5:5432"), "orders.example.com")

	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
	if st.Message() != "internal error" {
		t.Errorf("message = %q, want %q", st.Message(), "internal error")
	}
	if strings.Contains(st.Message(), "10.0.1.5") {
		t.Error("the status message leaks an internal hostname")
	}
}

// TestExtraDetailsSurvive proves WithDetails reaches the wire, which is what makes
// validation failures and retry hints machine-readable.
func TestExtraDetailsSurvive(t *testing.T) {
	t.Parallel()

	appErr := New(KindInvalidArgument, "VALIDATION_FAILED", "bad request").
		WithDetails(&errdetails.BadRequest{
			FieldViolations: []*errdetails.BadRequest_FieldViolation{
				{Field: "order.customer_id", Description: "must not be empty"},
			},
		})

	st := ToStatus(appErr, "orders.example.com")

	var br *errdetails.BadRequest
	for _, d := range st.Details() {
		if v, ok := d.(*errdetails.BadRequest); ok {
			br = v
		}
	}
	if br == nil {
		t.Fatal("BadRequest detail did not reach the status")
	}
	if got := br.GetFieldViolations()[0].GetField(); got != "order.customer_id" {
		t.Errorf("field = %q, want order.customer_id", got)
	}
}

// TestToErrorRoundTripsThroughGRPC proves the status survives a real client/server
// boundary rather than only looking right in-process.
func TestToErrorRoundTripsThroughGRPC(t *testing.T) {
	t.Parallel()

	err := ToError(New(KindAlreadyExists, "ORDER_ALREADY_EXISTS", "duplicate"), "orders")

	st, ok := status.FromError(err)
	if !ok {
		t.Fatal("ToError did not produce a gRPC status")
	}
	if st.Code() != codes.AlreadyExists {
		t.Errorf("code = %v, want AlreadyExists", st.Code())
	}
}

func TestKindRedactionBoundary(t *testing.T) {
	t.Parallel()

	// Exactly the two Kinds that mean "our fault, details are not the caller's business".
	for _, k := range AllKinds() {
		wantRedact := k == KindInternal || k == KindUnknown
		if got := k.Redacts(); got != wantRedact {
			t.Errorf("%s.Redacts() = %v, want %v", k, got, wantRedact)
		}

		e := New(k, "SOME_REASON", "a specific operator message")
		msg := e.ClientMessage()
		if wantRedact && msg != "internal error" {
			t.Errorf("%s: ClientMessage() = %q, want redaction", k, msg)
		}
		if !wantRedact && msg != "a specific operator message" {
			t.Errorf("%s: ClientMessage() = %q, want the original message", k, msg)
		}
	}
}

// TestNoCodeMapsToTwoStatuses makes an existing claim true.
//
// The comment above HTTPStatusFromCode said "mapping_test.go asserts that no code maps to two
// different statuses". It did not. Nothing did -- found while building the client's reverse
// mapping, which depends on exactly this property.
//
// It matters because HTTPStatusFromCode scans kindInfo, which is a Go MAP, and map iteration
// order is randomised per run. Two Kinds sharing a gRPC code but disagreeing on the HTTP
// status would make the same error return 404 or 409 depending on the run, and no test would
// fail twice in the same way. Several Kinds do legitimately share a code -- KindUnknown and
// KindInternal are both Internal -- so the invariant is not that codes are unique; it is that
// they agree.
func TestNoCodeMapsToTwoStatuses(t *testing.T) {
	t.Parallel()

	statusForCode := map[codes.Code]int{}
	kindForCode := map[codes.Code]Kind{}

	for _, kind := range AllKinds() {
		code, status := kind.GRPCCode(), kind.HTTPStatus()

		if seen, ok := statusForCode[code]; ok && seen != status {
			t.Errorf("%s maps to HTTP %d via %v and HTTP %d via %v.\n\n"+
				"HTTPStatusFromCode scans a map, so which one it returns depends on the "+
				"iteration order -- the same error would answer differently between runs, and "+
				"a test asserting either value would be flaky rather than wrong.",
				code, seen, kindForCode[code], status, kind)
			continue
		}
		statusForCode[code] = status
		kindForCode[code] = kind
	}

	if len(statusForCode) == 0 {
		t.Fatal("no kinds were examined, so this guard is vacuous")
	}

	// And the function agrees with the table it is derived from.
	for code, want := range statusForCode {
		if got := HTTPStatusFromCode(code); got != want {
			t.Errorf("HTTPStatusFromCode(%s) = %d, want %d", code, got, want)
		}
	}
}
