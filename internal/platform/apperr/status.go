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

	details := []protoadapt.MessageV1{
		protoadapt.MessageV1Of(&errdetails.ErrorInfo{
			Reason:   ae.Reason,
			Domain:   domain,
			Metadata: ae.Metadata,
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
