package grpcapi

import (
	"errors"

	"github.com/example/gomicro/internal/order"
	"github.com/example/gomicro/internal/platform/apperr"
)

// DomainErrorKind translates domain sentinel errors into transport-agnostic Kinds.
//
// This function is the entire reason internal/order can stay free of gRPC: the domain
// says "not found", and this boundary decides that means codes.NotFound and HTTP 404.
//
// The default is KindInternal, deliberately. An unrecognised error means a code path
// nobody classified, and reporting that as a 500 is the safe direction -- the alternative
// is guessing a client-visible code for a failure you do not understand.
func DomainErrorKind(err error) apperr.Kind {
	switch {
	case err == nil:
		return apperr.KindUnknown

	case errors.Is(err, order.ErrNotFound):
		return apperr.KindNotFound

	case errors.Is(err, order.ErrDuplicate):
		return apperr.KindAlreadyExists

	// A well-formed request that is illegal in the CURRENT state. Distinct from
	// InvalidArgument because retrying after the order changes state may succeed.
	case errors.Is(err, order.ErrInvalidTransition):
		return apperr.KindFailedPrecondition

	case errors.Is(err, order.ErrNoItems),
		errors.Is(err, order.ErrInvalidItem),
		errors.Is(err, order.ErrInvalidPageToken),
		errors.Is(err, order.ErrCurrencyMismatch),
		errors.Is(err, order.ErrInvalidCurrencyCode),
		errors.Is(err, order.ErrInvalidMoney),
		errors.Is(err, order.ErrMoneyOverflow):
		return apperr.KindInvalidArgument

	// A missing tenant means the auth interceptor did not populate one. That is a
	// programming or configuration error on OUR side, never the caller's fault, so it
	// must not come back as InvalidArgument and invite the client to retry with a
	// tenant_id in the body.
	case errors.Is(err, order.ErrMissingTenant):
		return apperr.KindInternal

	default:
		return apperr.KindInternal
	}
}

// reasonFor gives each domain failure a stable machine-readable code. Clients branch on
// these, never on the message text.
func reasonFor(err error) string {
	switch {
	case errors.Is(err, order.ErrNotFound):
		return "ORDER_NOT_FOUND"
	case errors.Is(err, order.ErrDuplicate):
		return "ORDER_ALREADY_EXISTS"
	case errors.Is(err, order.ErrInvalidTransition):
		return "ORDER_INVALID_STATUS_TRANSITION"
	case errors.Is(err, order.ErrNoItems):
		return "ORDER_HAS_NO_ITEMS"
	case errors.Is(err, order.ErrInvalidItem):
		return "ORDER_ITEM_INVALID"
	case errors.Is(err, order.ErrInvalidPageToken):
		return "PAGE_TOKEN_INVALID"
	case errors.Is(err, order.ErrCurrencyMismatch):
		return "CURRENCY_MISMATCH"
	case errors.Is(err, order.ErrInvalidCurrencyCode),
		errors.Is(err, order.ErrInvalidMoney),
		errors.Is(err, order.ErrMoneyOverflow):
		return "MONEY_INVALID"
	case errors.Is(err, order.ErrMissingTenant):
		return "TENANT_MISSING"
	default:
		return "INTERNAL"
	}
}

// asAppError converts any error from the domain into an *apperr.Error.
//
// The original error is kept as the wrapped cause so it reaches the log in full, while
// ClientMessage() decides what the caller is allowed to see.
func asAppError(err error) *apperr.Error {
	if err == nil {
		return nil
	}
	if ae, ok := apperr.From(err); ok {
		return ae
	}
	kind := DomainErrorKind(err)
	return apperr.Wrap(err, kind, reasonFor(err), err.Error())
}
