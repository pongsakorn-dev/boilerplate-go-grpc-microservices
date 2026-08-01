package apperr

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
//  2. Every status carries ErrorInfo{Reason, Domain}. Reason is the stable code clients
//     branch on; the human-readable message explicitly is not a contract.
func ToStatus(err error, domain string) *status.Status {
	if err == nil {
		return status.New(codes.OK, "")
	}

	ae, ok := From(err)
	if !ok {
		ae = Wrap(err, KindInternal, "INTERNAL", err.Error())
	}

	st := status.New(ae.Kind.GRPCCode(), ae.ClientMessage())

	info := &errdetails.ErrorInfo{
		Reason:   ae.Reason,
		Domain:   domain,
		Metadata: ae.Metadata,
	}
	withDetails, detailErr := st.WithDetails(info)
	if detailErr != nil {
		// Attaching details can only fail if the detail message cannot be marshalled.
		// A degraded status beats dropping the error entirely.
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
