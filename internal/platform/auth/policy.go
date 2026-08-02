package auth

import (
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// Rule is the authorisation decision for one RPC.
//
// The zero Rule requires authentication and no scopes. That is deliberate: a Rule someone
// half-fills in is restrictive, not permissive.
type Rule struct {
	// Public waives authentication entirely. Reserve it for endpoints a caller genuinely
	// cannot hold a credential for -- a kubelet probing grpc.health.v1.Health is the
	// canonical case, and very nearly the only one.
	Public bool

	// Scopes the caller must hold. ALL of them, not any.
	//
	// "All" over "any" is a considered choice: "any" reads the same in code and is weaker
	// in effect, so a reviewer skimming a rule with three scopes cannot tell whether it
	// tightened or loosened access. A rule needing "any" semantics splits into two rules
	// or a broader scope, and the reviewer sees which.
	Scopes []string
}

// Policy maps a full gRPC method name -- "/order.v1.OrderService/CreateOrder" -- to its
// rule.
//
// DEFAULT-DENY. A method with no entry is refused. That is what makes the coverage check
// meaningful: it is not a lint that suggests you write a rule, it is the difference between
// an RPC working and not working.
type Policy map[string]Rule

// Check authorises a call. It returns nil to allow, or an error naming the reason.
//
// The caller maps every error to a gRPC code and must not forward these strings to a
// client: "requires scope orders:write" tells an attacker exactly what to go phishing for.
func (p Policy) Check(method string, principal Principal, authenticated bool) error {
	rule, ok := p[method]
	if !ok {
		// Unreachable in a server built by NewServer, which refuses to start unless the
		// policy covers every registered method (ValidateCoverage). Kept as a real branch
		// because "unreachable" is a property of today's wiring, and the failure mode if
		// that wiring changes must be denial rather than admission.
		return fmt.Errorf("no policy entry for %s", method)
	}

	if rule.Public {
		return nil
	}
	if !authenticated {
		return fmt.Errorf("%s requires authentication", method)
	}

	var missing []string
	for _, want := range rule.Scopes {
		if !principal.HasScope(want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s requires scope(s) %s", method, strings.Join(missing, ", "))
	}
	return nil
}

// ValidateCoverage reports every served method with no policy entry, and every policy entry
// for a method that is not served.
//
// This is called at server construction, not only from a test. A test proves the policy was
// complete when someone last ran it; this makes an incomplete policy unable to boot. The
// two failures it catches are opposites and both real:
//
//   - a new RPC registered without a rule would, without the check, be reachable only to
//     find out at runtime whether it defaults open or closed;
//   - a rule left behind after a rename silently protects nothing, and reads in review as
//     though it still does.
//
// The `declared` argument is what makes the two directions SATISFIABLE AT THE SAME TIME, and
// it exists because the first version was not.
//
// With only `served`, an RPC declared in a .proto but not registered on this server had no
// legal state: the coverage test demanded a rule for it, and adding that rule made this
// function call the rule stale and refuse to boot. The only escapes were to delete the rule
// (restoring exactly the silent gap the test exists to close) or to stop checking its package
// (which silently dropped that package's other RPCs too). A guard with no way to comply is a
// guard somebody deletes.
//
// So a rule counts as live if its method is served OR declared in one of this repo's own
// protos. Genuinely stale rules -- a method that exists nowhere, the rename case -- are still
// caught, because a renamed method appears in neither set.
func (p Policy) ValidateCoverage(served, declared []string) error {
	servedSet := make(map[string]bool, len(served))
	var uncovered []string
	for _, m := range served {
		servedSet[m] = true
		if _, ok := p[m]; !ok {
			uncovered = append(uncovered, m)
		}
	}

	known := make(map[string]bool, len(served)+len(declared))
	for _, m := range served {
		known[m] = true
	}
	for _, m := range declared {
		known[m] = true
	}

	var stale []string
	for m := range p {
		if !known[m] {
			stale = append(stale, m)
		}
	}

	if len(uncovered) == 0 && len(stale) == 0 {
		return nil
	}

	sort.Strings(uncovered)
	sort.Strings(stale)

	var b strings.Builder
	b.WriteString("authorisation policy does not match the served methods:\n")
	if len(uncovered) > 0 {
		b.WriteString("\n  served but NOT in the policy (these would be denied at runtime):\n")
		for _, m := range uncovered {
			b.WriteString("    " + m + "\n")
		}
		b.WriteString("\n  Add a Rule for each in internal/grpcapi/policy.go. If an RPC really is\n" +
			"  unauthenticated, say so with Rule{Public: true} -- an explicit decision that\n" +
			"  shows up in review, rather than an omission that does not.\n")
	}
	if len(stale) > 0 {
		b.WriteString("\n  in the policy and declared NOWHERE -- not served, and in no .proto\n" +
			"  (dead rules, almost certainly a rename):\n")
		for _, m := range stale {
			b.WriteString("    " + m + "\n")
		}
	}
	return fmt.Errorf("%s", b.String())
}

// MethodsInPackages enumerates every RPC declared in the given proto packages, from the
// global descriptor registry.
//
// WHY A FILTER, rather than "every service in protoregistry.GlobalFiles". The registry
// holds descriptors for everything LINKED INTO the binary, which is not the same as
// everything this process serves. This repo imports the OTLP trace exporter, so
// opentelemetry.proto.collector.trace.v1.TraceService is in the registry -- a service we
// call as a client and never serve. Demanding a policy rule for it would be nonsense, and
// discovering that after writing the naive version is exactly why this takes a filter.
//
// The registry is still worth consulting alongside the server's own method list, because it
// sees RPCs that exist in the .proto and were generated but never wired up -- which the
// server cannot report, since it does not know they exist.
func MethodsInPackages(packages ...string) []string {
	want := make(map[string]bool, len(packages))
	for _, p := range packages {
		want[p] = true
	}

	var out []string
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if !want[string(fd.Package())] {
			return true
		}
		services := fd.Services()
		for i := range services.Len() {
			svc := services.Get(i)
			methods := svc.Methods()
			for j := range methods.Len() {
				out = append(out, fmt.Sprintf("/%s/%s", svc.FullName(), methods.Get(j).Name()))
			}
		}
		return true
	})

	sort.Strings(out)
	return out
}
