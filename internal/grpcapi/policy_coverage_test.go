package grpcapi

import (
	"slices"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/platform/auth"
)

// THE COVERAGE TESTS.
//
// Between them these assert that the set of RPCs this service exposes and the set of RPCs
// with an authorisation decision are the same set. A new method physically cannot ship
// without someone choosing a rule for it.
//
// Three checks, not one, because "what does this service expose" has two different answers
// and each catches something the other cannot:
//
//	served     what a built grpc.Server will dispatch, from GetServiceInfo. Ground truth,
//	           and the only source that knows about health and reflection -- registered by
//	           grpc-go, mentioned in no .proto here.
//	declared   what our .proto files define, from the descriptor registry. Sees an RPC that
//	           was generated but never wired up, which the server cannot report because it
//	           does not know it exists.
//
// The registry needed a filter to be usable at all: it holds descriptors for everything
// LINKED INTO the binary, and this repo imports the OTLP trace exporter, so
// opentelemetry.proto.collector.trace.v1.TraceService is in there -- a service we call as a
// client and never serve. The naive "every service in GlobalFiles" version demanded an
// authorisation rule for the OTLP collector.

// TestPolicyCoversEveryDeclaredRPC catches an RPC added to a .proto and generated but never
// registered on a server.
//
// GetServiceInfo cannot see those, so without this check the gap opens silently: the method
// exists in the generated code and in any client stub built from it, and the day someone
// wires it up it inherits whatever the default is. Here the default is denial -- but the
// server also refuses to start, which is a confusing failure to hit at deploy time rather
// than at test time.
func TestPolicyCoversEveryDeclaredRPC(t *testing.T) {
	t.Parallel()

	declared := declaredMethods(t)

	policy := DefaultPolicy()
	for _, method := range declared {
		if _, ok := policy[method]; !ok {
			t.Errorf("%s is declared in a .proto but has no authorisation rule.\n\n"+
				"Add one to DefaultPolicy in policy.go. If it really is unauthenticated, say so "+
				"with Rule{Public: true} -- an explicit decision a reviewer can see, rather than "+
				"an omission they cannot.", method)
		}
	}
}

// TestPolicyRulesAllReferenceRealMethods is the opposite direction: a rule for a method that
// no longer exists.
//
// A stale rule is worse than a missing one. It reads in review as though it protects
// something, so a reviewer checking "is CancelOrder locked down?" finds a line saying yes,
// for a method renamed three months ago.
func TestPolicyRulesAllReferenceRealMethods(t *testing.T) {
	t.Parallel()

	declared := declaredMethods(t)

	// grpc-go registers these; they appear in no .proto in this repo, so they are legitimate
	// policy entries with no declared counterpart. Listed explicitly rather than matched by
	// prefix so that adding a rule for some other unowned service still fails this test.
	infrastructure := []string{
		"/grpc.health.v1.Health/Check",
		"/grpc.health.v1.Health/Watch",
		"/grpc.health.v1.Health/List",
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
	}

	for method := range DefaultPolicy() {
		if slices.Contains(declared, method) || slices.Contains(infrastructure, method) {
			continue
		}
		t.Errorf("the policy has a rule for %s, which no .proto declares and which is not one "+
			"of the services grpc-go registers.\n\n"+
			"Most likely a rename left it behind. A rule for a method nobody serves protects "+
			"nothing while reading as though it does.", method)
	}
}

// TestPolicyIsScopedNotBlanket checks the rules say something.
//
// A policy where every entry is Public, or every entry has no scopes, passes both coverage
// tests above perfectly and authorises nothing. Coverage proves a decision was MADE; this
// proves the decisions are not uniformly the weakest one available.
func TestPolicyIsScopedNotBlanket(t *testing.T) {
	t.Parallel()

	policy := DefaultPolicy()

	var public, scoped int
	for method, rule := range policy {
		switch {
		case rule.Public:
			public++
			// Only health may be anonymous. It has to be -- a kubelet holds no credential --
			// but the exemption must stay exactly that size. A Public rule appearing on a
			// business RPC is how an endpoint quietly becomes anonymous.
			if !strings.HasPrefix(method, "/grpc.health.v1.Health/") {
				t.Errorf("%s is Public. Only the health service may be anonymous; everything "+
					"else must at least require a verified caller.", method)
			}
		case len(rule.Scopes) > 0:
			scoped++
		}
	}

	// Every business RPC must be scope-gated. Requiring merely "a valid token" means any
	// caller with any credential from your IdP can do anything -- which in a shared-IdP
	// estate includes services and users with no relationship to this one.
	for _, method := range auth.MethodsInPackages(ownedProtoPackages...) {
		if rule := policy[method]; len(rule.Scopes) == 0 {
			t.Errorf("%s requires no scopes, so any authenticated caller may invoke it.\n\n"+
				"On a shared identity provider that includes every user and service in the "+
				"estate, related to this one or not.", method)
		}
	}

	if scoped == 0 {
		t.Fatal("no rule requires any scope; the policy authorises nothing in particular")
	}
}

// TestReadAndWriteScopesAreActuallySeparated verifies the split is real.
//
// Separating orders:read from orders:write is only worth the extra scope if a read-only
// credential genuinely cannot mutate. If every rule listed both, the split would be
// documentation rather than a control -- and would read exactly the same in review.
func TestReadAndWriteScopesAreActuallySeparated(t *testing.T) {
	t.Parallel()

	policy := DefaultPolicy()
	readOnly := auth.Principal{Subject: "reporting", TenantID: "t", Scopes: []string{ScopeOrdersRead}}

	mutations := []string{
		"/order.v1.OrderService/CreateOrder",
		"/order.v1.OrderService/CancelOrder",
	}
	for _, method := range mutations {
		if err := policy.Check(method, readOnly, true); err == nil {
			t.Errorf("a credential holding only %s was allowed to call %s.\n\n"+
				"The read/write split exists so an analytics or reporting client can be issued "+
				"a token that provably cannot mutate. If this passes, it cannot.", ScopeOrdersRead, method)
		}
	}

	reads := []string{
		"/order.v1.OrderService/GetOrder",
		"/order.v1.OrderService/ListOrders",
		"/order.v1.OrderService/WatchOrders",
	}
	for _, method := range reads {
		if err := policy.Check(method, readOnly, true); err != nil {
			t.Errorf("a read-only credential was denied %s: %v", method, err)
		}
	}
}

// TestCancelOrderIsTreatedAsAWrite pins the one rule most likely to be miscategorised.
//
// "Cancel" reads like a lifecycle operation rather than a mutation, and grouping it with the
// reads is an easy mistake with an expensive consequence: a reporting credential that can
// cancel customer orders.
func TestCancelOrderIsTreatedAsAWrite(t *testing.T) {
	t.Parallel()

	rule := DefaultPolicy()["/order.v1.OrderService/CancelOrder"]
	if !slices.Contains(rule.Scopes, ScopeOrdersWrite) {
		t.Errorf("CancelOrder requires %v, not %s. Cancelling an order mutates it.",
			rule.Scopes, ScopeOrdersWrite)
	}
}

// declaredMethods is MethodsInPackages with a PER-PACKAGE vacuity check.
//
// The aggregate check it replaces -- "len(declared) != 0" -- is only sound while
// ownedProtoPackages has exactly one entry. With two or more, a package whose descriptors are
// not linked into this test binary contributes zero methods and the total stays comfortably
// non-zero, so its RPCs are never checked for an authorisation decision and every test here
// still passes green.
//
// That is precisely the "generated but never wired up" case this file exists to catch, and it
// is the same quiet-narrowing failure that motivated TestOwnedProtoPackagesMatchTheProtoDirectory:
// that test guards the NAMES in the list, this guards that each name resolves to something.
func declaredMethods(t *testing.T) []string {
	t.Helper()

	var all []string
	for _, pkg := range ownedProtoPackages {
		methods := auth.MethodsInPackages(pkg)
		if len(methods) == 0 {
			t.Fatalf("proto package %q contributed no RPCs to the descriptor registry.\n\n"+
				"Its generated package is not linked into this test binary, so every coverage "+
				"assertion about it is vacuously true -- its RPCs could have no authorisation "+
				"rule at all and this file would stay green. Add a blank import of its generated "+
				"package to internal/grpcapi, or register its service on the server.", pkg)
		}
		all = append(all, methods...)
	}

	if len(all) == 0 {
		t.Fatalf("no RPCs found in %v at all", ownedProtoPackages)
	}
	return all
}
