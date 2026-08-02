package client

import (
	"context"
	"strings"

	"google.golang.org/grpc/metadata"

	"github.com/example/gomicro/internal/platform/auth"
)

// Metadata keys carried across a hop.
//
// These are CONTEXT, NOT CREDENTIALS. Nothing on the receiving side trusts them: this
// template's server builds its Principal from a verified token and from nothing else, so a
// caller that forges x-gomicro-tenant-id gains exactly nothing. They exist so that a log line
// or a trace on the far side can say which tenant a piece of work was ultimately for, which is
// otherwise unknowable once a request crosses a service boundary.
//
// If you ever find yourself reading one of these on the server to make an authorisation
// decision, that is the bug. The tenant used for authorisation comes from the token.
const (
	MetadataTenantID = "x-gomicro-tenant-id"
	MetadataSubject  = "x-gomicro-subject"

	// authorizationKey must be lowercase. gRPC metadata keys are canonicalised to lowercase
	// on the wire, and a map built by hand with "Authorization" produces a key the callee's
	// md.Get("authorization") never finds -- so the call silently becomes anonymous. Under
	// AUTH_MODE=dev it then succeeds anyway, which is how this stays hidden until production.
	authorizationKey = "authorization"
)

// Credentials supplies the token this SERVICE authenticates with.
//
// # Why the caller's token is not forwarded
//
// The obvious implementation takes the inbound Authorization header and puts it on the
// outbound call. It is wrong in two ways that are hard to reverse once shipped.
//
// The first is audience. A token is minted for a specific audience, and the verifier here
// refuses a token whose aud does not name it -- that check is the reason an attacker who
// steals a token for one service cannot spend it at another. Forwarding a user's token to a
// second service only works if both services share an audience, which means the check is no
// longer distinguishing anything, for every service in the estate.
//
// The second is the confused deputy. A forwarded user token is a bearer token: whoever holds
// it can act as that user, anywhere it is accepted, for as long as it lives. A downstream that
// receives one can call a THIRD service as the user, and nothing in the request says it was
// not the user who asked. Compromise anything holding forwarded tokens and you have every
// user who passed through it.
//
// The model that does work is a service credential plus a separate, verifiable statement about
// the end user -- the caller authenticates as itself, and asserts on the user's behalf. That
// assertion needs an issuer this template does not ship, so what is shipped is the seam:
// Credentials for the service's own identity, and the metadata above for context that nothing
// trusts. The wrong thing is not merely undocumented; it is absent, and
// TestTheCallersTokenIsNeverForwarded keeps it that way.
type Credentials interface {
	// Token returns the bearer token for one call, without the "Bearer " prefix.
	//
	// It takes a context so an implementation can refresh against a token endpoint and
	// respect the caller's deadline. Returning an error fails the call rather than sending
	// it unauthenticated, because an upstream that answers an anonymous request is an
	// upstream whose auth is broken -- and quietly succeeding would hide that.
	Token(ctx context.Context) (string, error)
}

// StaticToken is a Credentials that always returns the same token.
//
// Useful in tests and for a service whose credential is mounted as a file and refreshed out of
// process. It is not a token MANAGER: nothing here refreshes, caches or checks expiry.
type StaticToken string

// Token returns the token.
func (t StaticToken) Token(context.Context) (string, error) { return string(t), nil }

// attachIdentity puts the service credential and the caller's context on an outbound call.
func (c *Options) attachIdentity(ctx context.Context) (context.Context, error) {
	// STRIP FIRST, unconditionally.
	//
	// Outgoing metadata is inherited when a handler passes its own context to a client call,
	// and the gateway explicitly copies inbound headers onto its outgoing context. Without
	// this, a caller's Authorization header rides along by default and every argument in the
	// doc comment above is quietly untrue. Deleting a key that is not there is free.
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		if _, present := md[authorizationKey]; present {
			stripped := md.Copy()
			stripped.Delete(authorizationKey)
			ctx = metadata.NewOutgoingContext(ctx, stripped)
		}
	}

	if principal, ok := auth.PrincipalFrom(ctx); ok {
		if principal.TenantID != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, MetadataTenantID, principal.TenantID)
		}
		if principal.Subject != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, MetadataSubject, principal.Subject)
		}
	}

	if c.Credentials == nil {
		return ctx, nil
	}

	token, err := c.Credentials.Token(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) == "" {
		return ctx, nil
	}

	return metadata.AppendToOutgoingContext(ctx, authorizationKey, "Bearer "+token), nil
}
