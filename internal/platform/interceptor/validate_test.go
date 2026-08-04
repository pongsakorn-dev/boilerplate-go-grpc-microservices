package interceptor

import (
	"context"
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/genproto/googleapis/rpc/errdetails"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
	"github.com/example/gomicro/internal/platform/apperr"
)

func newValidator(t *testing.T) protovalidate.Validator {
	t.Helper()
	v, err := protovalidate.New()
	if err != nil {
		t.Fatalf("protovalidate.New: %v", err)
	}
	return v
}

// TestValidateTranslatesToStandardBadRequest asserts the TRANSLATED shape, not
// protovalidate's internal one.
//
// This matters because protovalidate's violation representation is an implementation
// detail that has changed across versions. google.rpc.BadRequest is the type every gRPC
// client library and API gateway already understands, and pinning it here means a
// protovalidate upgrade cannot silently change the error contract clients depend on.
func TestValidateTranslatesToStandardBadRequest(t *testing.T) {
	t.Parallel()

	intercept := Validate(newValidator(t))

	var handlerRan bool
	handler := func(ctx context.Context, req any) (any, error) {
		handlerRan = true
		return "ok", nil
	}

	// customer_id is empty and items is empty: both violate the .proto rules.
	req := &orderv1.CreateOrderRequest{}

	_, err := intercept(context.Background(), req, testInfo, handler)

	if err == nil {
		t.Fatal("an invalid request was accepted")
	}
	if handlerRan {
		t.Error("the handler ran for an invalid request; validation must reject before business logic")
	}
	if kind := apperr.KindOf(err); kind != apperr.KindInvalidArgument {
		t.Errorf("kind = %v, want InvalidArgument", kind)
	}

	appErr, ok := apperr.From(err)
	if !ok {
		t.Fatal("not an apperr.Error")
	}

	var br *errdetails.BadRequest
	for _, d := range appErr.Details {
		if v, ok := d.(*errdetails.BadRequest); ok {
			br = v
		}
	}
	if br == nil {
		t.Fatal("no BadRequest detail; the client cannot tell WHICH field was wrong")
	}
	if len(br.GetFieldViolations()) == 0 {
		t.Fatal("BadRequest carries no field violations")
	}

	// The field path must be usable by a client to highlight an input.
	seen := map[string]bool{}
	for _, fv := range br.GetFieldViolations() {
		if fv.GetField() == "" {
			t.Error("a field violation has an empty field path")
		}
		if fv.GetDescription() == "" {
			t.Error("a field violation has no description")
		}
		// Reason carries the stable rule id (e.g. "string.min_len"), which is what a
		// client branches on -- never the human-readable description.
		if fv.GetReason() == "" {
			t.Errorf("field %q has no rule id in Reason", fv.GetField())
		}
		seen[fv.GetField()] = true
	}

	if !seen["customer_id"] && !seen["items"] {
		t.Errorf("expected violations on customer_id or items, got %v", seen)
	}
}

func TestValidateAcceptsAValidRequest(t *testing.T) {
	t.Parallel()

	intercept := Validate(newValidator(t))

	handler := func(ctx context.Context, req any) (any, error) { return "ok", nil }

	req := &orderv1.CreateOrderRequest{
		CustomerId: "customer-1",
		Items: []*orderv1.OrderItem{{
			Sku:       "SKU-1",
			Quantity:  1,
			UnitPrice: &orderv1.Money{CurrencyCode: "USD", Units: 10},
		}},
	}

	resp, err := intercept(context.Background(), req, testInfo, handler)
	if err != nil {
		t.Fatalf("a valid request was rejected: %v", err)
	}
	if resp != "ok" {
		t.Errorf("resp = %v, want ok", resp)
	}
}

// TestValidateEnforcesRulesDeclaredInTheProto proves the rules genuinely come from the
// .proto rather than from hand-written Go -- which is the entire argument for protovalidate
// over writing validation in the handler.
func TestValidateEnforcesRulesDeclaredInTheProto(t *testing.T) {
	t.Parallel()

	intercept := Validate(newValidator(t))
	handler := func(ctx context.Context, req any) (any, error) { return "ok", nil }

	cases := []struct {
		name string
		req  *orderv1.CreateOrderRequest
	}{
		{
			name: "zero quantity violates int32.gt",
			req: &orderv1.CreateOrderRequest{
				CustomerId: "c",
				Items: []*orderv1.OrderItem{{
					Sku: "SKU-1", Quantity: 0,
					UnitPrice: &orderv1.Money{CurrencyCode: "USD", Units: 1},
				}},
			},
		},
		{
			name: "lowercase currency violates the pattern",
			req: &orderv1.CreateOrderRequest{
				CustomerId: "c",
				Items: []*orderv1.OrderItem{{
					Sku: "SKU-1", Quantity: 1,
					UnitPrice: &orderv1.Money{CurrencyCode: "usd", Units: 1},
				}},
			},
		},
		{
			name: "missing unit price violates required",
			req: &orderv1.CreateOrderRequest{
				CustomerId: "c",
				Items:      []*orderv1.OrderItem{{Sku: "SKU-1", Quantity: 1}},
			},
		},
		{
			name: "a non-uuid idempotency key violates string.uuid",
			req: &orderv1.CreateOrderRequest{
				CustomerId:     "c",
				IdempotencyKey: "not-a-uuid",
				Items: []*orderv1.OrderItem{{
					Sku: "SKU-1", Quantity: 1,
					UnitPrice: &orderv1.Money{CurrencyCode: "USD", Units: 1},
				}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := intercept(context.Background(), tc.req, testInfo, handler); err == nil {
				t.Error("the request was accepted, but the .proto forbids it")
			}
		})
	}
}

// TestAnOversizedPageSizeIsRejectedNotClamped settles which of two mechanisms a caller meets.
//
// order.ClampPageSize reduces anything above MaxPageSize to MaxPageSize, and the .proto puts
// `lte: 1000` on the same field. Both cannot be what a client experiences, and the .proto
// comment claimed the wrong one: it said an oversized request was "silently reduced to 1000"
// and that "a caller asking for 10000 receives 1000 rows". Validation runs first, so they
// receive an error and no rows at all.
//
// That distinction is the whole difference between a forgiving API and a strict one, it is
// what a client's error handling has to be written against, and the sentence asserting it
// shipped into the generated Go and the published OpenAPI document.
//
// The clamp is not dead code -- it still supplies the 0 -> DefaultPageSize fallback, and it
// still guards the domain against a caller that reaches order.Service directly rather than
// over the wire.
func TestAnOversizedPageSizeIsRejectedNotClamped(t *testing.T) {
	t.Parallel()

	intercept := Validate(newValidator(t))

	var reached bool
	handler := func(ctx context.Context, req any) (any, error) {
		reached = true
		return "ok", nil
	}

	req := &orderv1.ListOrdersRequest{PageSize: 10_000}

	_, err := intercept(context.Background(), req, testInfo, handler)
	if err == nil {
		t.Fatal("page_size 10000 was accepted; the .proto declares lte: 1000")
	}
	if reached {
		t.Error("the handler ran, so the clamp -- not validation -- would decide the outcome")
	}
	if kind := apperr.KindOf(err); kind != apperr.KindInvalidArgument {
		t.Errorf("kind = %v, want InvalidArgument", kind)
	}

	// And the boundary itself is accepted, or the documented maximum would be off by one.
	reached = false
	if _, err := intercept(context.Background(),
		&orderv1.ListOrdersRequest{PageSize: 1000}, testInfo, handler); err != nil {
		t.Errorf("page_size 1000 was rejected, but 1000 is the documented maximum: %v", err)
	}
	if !reached {
		t.Error("the handler did not run for the boundary value")
	}
}

// TestValidateIgnoresAnEmptyOptionalField proves IGNORE_IF_ZERO_VALUE works: an absent
// idempotency key must be allowed, while a malformed one must not.
func TestValidateIgnoresAnEmptyOptionalField(t *testing.T) {
	t.Parallel()

	intercept := Validate(newValidator(t))
	handler := func(ctx context.Context, req any) (any, error) { return "ok", nil }

	req := &orderv1.CreateOrderRequest{
		CustomerId:     "c",
		IdempotencyKey: "", // absent, not malformed
		Items: []*orderv1.OrderItem{{
			Sku: "SKU-1", Quantity: 1,
			UnitPrice: &orderv1.Money{CurrencyCode: "USD", Units: 1},
		}},
	}

	if _, err := intercept(context.Background(), req, testInfo, handler); err != nil {
		t.Errorf("an absent optional field was rejected: %v", err)
	}
}

// TestValidatePassesThroughNonProtoRequests keeps the interceptor usable in tests that
// pass fakes, rather than failing closed on a case that cannot occur over gRPC.
func TestValidatePassesThroughNonProtoRequests(t *testing.T) {
	t.Parallel()

	intercept := Validate(newValidator(t))
	handler := func(ctx context.Context, req any) (any, error) { return "ok", nil }

	if _, err := intercept(context.Background(), "not a proto", testInfo, handler); err != nil {
		t.Errorf("a non-proto request was rejected: %v", err)
	}
}
