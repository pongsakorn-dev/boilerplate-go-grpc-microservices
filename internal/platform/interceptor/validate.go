package interceptor

import (
	"context"
	"errors"

	"buf.build/go/protovalidate"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/example/gomicro/internal/platform/apperr"
)

// Validate enforces the constraints declared in the .proto files.
//
// protovalidate, not protoc-gen-validate: PGV is explicitly superseded, and protovalidate
// evaluates CEL expressions against the message descriptor at RUNTIME rather than
// generating Go validation code. The practical difference is that the rules live in the
// .proto, so a Python or TypeScript client gets the same constraints from the same source
// of truth instead of reimplementing them.
//
// It sits near the innermost position in the chain: after auth (an unauthenticated caller
// should get Unauthenticated, not a detailed critique of their malformed payload -- that
// is a free schema-discovery oracle) and outside errmap (so its errors are mapped like any
// other).
func Validate(validator protovalidate.Validator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		msg, ok := req.(proto.Message)
		if !ok {
			// Not a proto message: nothing to validate. This cannot happen over gRPC, but
			// failing closed on an impossible case would break tests that pass fakes.
			return handler(ctx, req)
		}

		if err := validator.Validate(msg); err != nil {
			return nil, toBadRequest(err)
		}
		return handler(ctx, req)
	}
}

// toBadRequest converts protovalidate's error into the STANDARD google.rpc.BadRequest.
//
// This translation is the point. protovalidate's own violation shape is an implementation
// detail that has changed across versions; google.rpc.BadRequest is the type every gRPC
// client library, every API gateway, and every Google-style API already understands.
//
// It also means validation failures and domain failures present ONE detail shape to
// clients, rather than the caller having to parse two different error formats depending on
// which layer rejected them.
func toBadRequest(err error) error {
	var ve *protovalidate.ValidationError
	if !errors.As(err, &ve) {
		// A compilation or runtime error in the CEL expressions is OUR bug, not the
		// caller's -- returning InvalidArgument would blame the client for a broken rule.
		return apperr.Wrap(err, apperr.KindInternal, "VALIDATION_ENGINE_ERROR",
			"validation engine failed")
	}

	br := &errdetails.BadRequest{}
	for _, v := range ve.Violations {
		br.FieldViolations = append(br.FieldViolations, &errdetails.BadRequest_FieldViolation{
			Field:       protovalidate.FieldPathString(v.Proto.GetField()),
			Description: v.Proto.GetMessage(),
			// Reason carries the rule id (e.g. "string.uuid"), which is stable and
			// machine-readable -- clients branch on this, never on Description.
			Reason: v.Proto.GetRuleId(),
		})
	}

	return apperr.New(apperr.KindInvalidArgument, "VALIDATION_FAILED", "request failed validation").
		WithDetails(br)
}

// isServerFault reports whether a status code indicates OUR failure rather than the
// caller's.
//
// The split drives log level. A NotFound or InvalidArgument logged at ERROR is noise: it
// means the client asked for something wrong, which is a normal event in any public API.
// Logging those at ERROR is how a team learns to ignore ERROR entirely, and then misses the
// one that mattered.
func isServerFault(code codes.Code) bool {
	switch code {
	case codes.Internal, codes.Unknown, codes.DataLoss, codes.Unavailable:
		return true
	default:
		return false
	}
}
