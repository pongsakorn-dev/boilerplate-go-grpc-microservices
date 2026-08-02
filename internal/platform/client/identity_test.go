package client_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/example/gomicro/internal/platform/auth"
	"github.com/example/gomicro/internal/platform/client"
)

// TestTheCallersTokenIsNeverForwarded is the security assertion this package exists to make
// unfalsifiable.
//
// Forwarding the inbound Authorization header is the obvious implementation, it works on the
// first try, and it is wrong in two ways that are expensive to reverse:
//
//   - AUDIENCE. A token names the service it may be spent at, and the verifier refuses one
//     that names someone else -- which is exactly why a token stolen from service A cannot be
//     used against service B. Forwarding only works if every service shares an audience, at
//     which point that check distinguishes nothing, everywhere.
//   - CONFUSED DEPUTY. A forwarded bearer token lets whoever holds it act as that user for as
//     long as it lives. A compromised downstream can spend it against a third service, and
//     nothing in the request says the user did not ask.
//
// The header arrives on the outgoing context by inheritance -- a handler calling an upstream
// naturally passes its own context along -- so NOT forwarding takes deliberate code. This
// test is what stops that code being deleted as redundant.
func TestTheCallersTokenIsNeverForwarded(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	conn := up.dial(t)

	// Exactly the shape a handler produces: the inbound credential, already on the outgoing
	// context, plus a verified principal.
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		"authorization", "Bearer the-end-users-token")
	ctx = auth.WithPrincipal(ctx, auth.Principal{Subject: "user-1", TenantID: "acme"})

	if err := invoke(ctx, conn, "order.v1.OrderService/GetOrder"); err != nil {
		t.Fatalf("call: %v", err)
	}

	calls := up.observedCalls()
	if len(calls) != 1 {
		t.Fatalf("upstream saw %d calls, want 1", len(calls))
	}

	for _, got := range calls[0].md.Get("authorization") {
		if strings.Contains(got, "the-end-users-token") {
			t.Errorf("the caller's token reached the upstream: %q\n\n"+
				"It must be stripped and replaced by this SERVICE's own credential. See the "+
				"Credentials doc comment for why forwarding defeats audience validation and "+
				"turns every downstream into a confused deputy.", got)
		}
	}
}

// TestTheServiceCredentialIsSent is the other half: stripping the caller's token is only
// correct if the service sends its own.
func TestTheServiceCredentialIsSent(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	conn := up.dial(t, func(o *client.Options) {
		o.Credentials = client.StaticToken("this-services-own-token")
	})

	ctx := metadata.AppendToOutgoingContext(context.Background(),
		"authorization", "Bearer the-end-users-token")

	if err := invoke(ctx, conn, "order.v1.OrderService/GetOrder"); err != nil {
		t.Fatalf("call: %v", err)
	}

	got := up.observedCalls()[0].md.Get("authorization")
	if len(got) != 1 {
		t.Fatalf("upstream saw %d authorization values, want exactly 1: %v\n\n"+
			"More than one means the caller's token was APPENDED rather than replaced, and a "+
			"callee reading values[0] may well pick the wrong one.", len(got), got)
	}
	if got[0] != "Bearer this-services-own-token" {
		t.Errorf("authorization = %q, want this service's own credential", got[0])
	}
}

// TestTheCallersTenantTravelsAsContextNotCredential pins what IS forwarded.
//
// The tenant is worth carrying: without it a log line on the far side cannot say whose work it
// was doing, and that is unrecoverable after the fact. It travels under a name that does not
// look like a credential, because nothing on the receiving side may treat it as one -- the
// server builds its Principal from a verified token and from nothing else.
func TestTheCallersTenantTravelsAsContextNotCredential(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	conn := up.dial(t)

	ctx := auth.WithPrincipal(context.Background(),
		auth.Principal{Subject: "user-1", TenantID: "acme", Scopes: []string{"orders:read"}})

	if err := invoke(ctx, conn, "order.v1.OrderService/GetOrder"); err != nil {
		t.Fatalf("call: %v", err)
	}

	md := up.observedCalls()[0].md
	if got := md.Get(client.MetadataTenantID); len(got) != 1 || got[0] != "acme" {
		t.Errorf("%s = %v, want [acme]", client.MetadataTenantID, got)
	}
	if got := md.Get(client.MetadataSubject); len(got) != 1 || got[0] != "user-1" {
		t.Errorf("%s = %v, want [user-1]", client.MetadataSubject, got)
	}

	// Scopes are deliberately NOT propagated. They are an authorisation input, and an
	// authorisation input that arrives as an unverified header is a privilege escalation
	// waiting for the first service that reads it.
	for k := range md {
		if strings.Contains(k, "scope") {
			t.Errorf("scopes were propagated as metadata (%q); they are an authorisation "+
				"input and must come from a verified token", k)
		}
	}
}

// TestAnAnonymousCallCarriesNoIdentityHeaders covers the path with no principal -- a
// background job, or a public method. It must not invent one.
func TestAnAnonymousCallCarriesNoIdentityHeaders(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	conn := up.dial(t)

	if err := invoke(context.Background(), conn, "order.v1.OrderService/ListOrders"); err != nil {
		t.Fatalf("call: %v", err)
	}

	md := up.observedCalls()[0].md
	if got := md.Get(client.MetadataTenantID); len(got) != 0 {
		t.Errorf("%s = %v on a call with no principal, want nothing", client.MetadataTenantID, got)
	}
	if got := md.Get("authorization"); len(got) != 0 {
		t.Errorf("authorization = %v with no credentials configured, want nothing", got)
	}
}
