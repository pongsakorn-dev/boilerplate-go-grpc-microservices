// Package auth carries caller identity through the request.
//
// The rule this package exists to enforce: the tenant comes from the VERIFIED token and
// never from the request body. A client that sends tenant_id in a message is ignored --
// otherwise "multi-tenant" means "any caller may name any tenant".
package auth

import "context"

// Principal is the authenticated caller.
type Principal struct {
	// Subject is the stable user or service identifier from the token's `sub` claim.
	Subject string

	// TenantID is the tenant this caller acts within. Everything downstream scopes to it.
	TenantID string

	// Scopes are the granted permissions.
	Scopes []string

	// IsService distinguishes a machine caller from an end user. Service-to-service calls
	// carry a service credential; they are not end users with unlimited rights.
	IsService bool
}

// HasScope reports whether the principal was granted a scope.
func (p Principal) HasScope(want string) bool {
	for _, s := range p.Scopes {
		if s == want {
			return true
		}
	}
	return false
}

// ctxKey is unexported so no other package can inject a Principal into a context.
// That matters: if the key were exported, any package could forge an identity by
// stuffing one into a context, and the auth interceptor would no longer be the only
// source of truth.
type ctxKey struct{}

// WithPrincipal returns a context carrying the verified principal. Only the auth
// interceptor should call this on the server side.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom returns the verified principal, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// TenantFrom returns the caller's tenant. The boolean is false when the request is
// unauthenticated, and callers must treat that as "deny", never as "all tenants".
func TenantFrom(ctx context.Context) (string, bool) {
	p, ok := PrincipalFrom(ctx)
	if !ok || p.TenantID == "" {
		return "", false
	}
	return p.TenantID, true
}
