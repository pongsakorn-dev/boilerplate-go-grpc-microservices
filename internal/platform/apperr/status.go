package apperr

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"
)

// ToStatus converts any error into a gRPC status carrying machine-readable details.
//
// Two rules are enforced here and nowhere else:
//
//  1. An unclassified error becomes codes.Internal with the literal message
//     "internal error". The cause is preserved for the LOG (the caller logs
//     err, not the status), but never for the client. Returning
//     `pq: duplicate key value violates unique constraint "users_email_key"`
//     leaks your schema and confirms an account exists.
//
//     "Unclassified" excludes context.Canceled and context.DeadlineExceeded, which
//     classify names explicitly. They are the caller's own doing, not a fault of
//     this service, and calling them Internal is both a lie to the client and a
//     permanent noise floor under the Internal error-rate alert.
//
//  2. Every status carries ErrorInfo{Reason, Domain}. Reason is the stable code clients
//     branch on; the human-readable message explicitly is not a contract.
func ToStatus(err error, domain string) *status.Status {
	if err == nil {
		return status.New(codes.OK, "")
	}

	ae, ok := From(err)
	if !ok {
		kind, reason, message := classify(err)
		ae = Wrap(err, kind, reason, message)
	}

	st := status.New(ae.Kind.GRPCCode(), ae.ClientMessage())

	// METADATA IS REDACTED ALONGSIDE THE MESSAGE, and omitting that was a hole in rule 1.
	//
	// ClientMessage() replaces the text of a redacting Kind with "internal error". Metadata
	// went out untouched, and it is the same wire object -- ErrorInfo carries reason, domain
	// AND metadata, so a caller reading the details of an Internal error saw whatever the
	// handler had attached for its own logs.
	//
	// It was not hypothetical. client.AsAppError attaches upstream, upstream_method and
	// upstream_code, and its Kind for anything that is not a plain outage is KindInternal --
	// so an untranslated upstream failure disclosed the address of an internal service and
	// the name of an internal RPC to an external caller. The comment on that function says it
	// relies on redaction to keep exactly that text out of the response.
	//
	// The information is not lost: Error() still carries the cause into the log, which is
	// where "which upstream, which method" belongs.
	metadata := ae.Metadata
	if ae.Kind.Redacts() {
		metadata = nil
	}

	details := []protoadapt.MessageV1{
		protoadapt.MessageV1Of(&errdetails.ErrorInfo{
			Reason:   ae.Reason,
			Domain:   domain,
			Metadata: metadata,
		}),
	}
	for _, d := range ae.Details {
		details = append(details, protoadapt.MessageV1Of(d))
	}

	withDetails, detailErr := st.WithDetails(details...)
	if detailErr != nil {
		// Attaching details can only fail if a detail message cannot be marshalled. A
		// status without its details still tells the caller the code and the reason, which
		// beats dropping the error entirely.
		return st
	}
	return withDetails
}

// ToError is ToStatus as an error value, for returning directly from a handler.
func ToError(err error, domain string) error {
	if err == nil {
		return nil
	}
	return ToStatus(err, domain).Err()
}
