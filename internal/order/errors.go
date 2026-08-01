package order

import "errors"

// Domain errors are plain sentinels, not gRPC statuses and not apperr values.
//
// This is what keeps internal/order free of any transport dependency: the mapping from
// these sentinels to gRPC codes and HTTP statuses lives at the boundary, in
// internal/grpcapi/errmap.go, and is pinned by an exhaustive table test.
//
// Callers compare with errors.Is, so implementations are free to wrap these with context
// ("order 123 not found") without breaking the mapping.
var (
	// ErrNotFound means no order matched the id within the caller's tenant. Note it is
	// deliberately indistinguishable from "exists but belongs to another tenant" -- a
	// distinguishable response would be a cross-tenant existence oracle.
	ErrNotFound = errors.New("order not found")

	// ErrDuplicate means a uniqueness constraint rejected the write.
	ErrDuplicate = errors.New("order already exists")

	// ErrInvalidTransition means the requested status change is not legal from the
	// order's current status.
	ErrInvalidTransition = errors.New("invalid status transition")

	// ErrNoItems means an order was submitted with no line items.
	ErrNoItems = errors.New("order must have at least one item")

	// ErrInvalidItem means a line item failed validation (blank SKU, non-positive
	// quantity, malformed price).
	ErrInvalidItem = errors.New("invalid order item")

	// ErrMissingTenant means no tenant reached the store. This is a programming error,
	// not a user error: the auth interceptor is supposed to have populated it, and the
	// store fails closed rather than returning every tenant's rows.
	ErrMissingTenant = errors.New("tenant id is required")

	// ErrInvalidPageToken means the opaque cursor did not decode, or was issued for a
	// different filter.
	ErrInvalidPageToken = errors.New("invalid page token")
)
