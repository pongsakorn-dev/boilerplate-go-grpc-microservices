package auth_test

import (
	"strings"
	"testing"

	"github.com/example/gomicro/internal/platform/auth"
)

// TestUnknownMethodsAreDenied is the whole thesis of the policy in one assertion.
//
// The alternative -- an unlisted method being allowed -- means every new RPC ships
// unprotected by default, and the gap is invisible: the endpoint works, tests pass, and
// nobody discovers the omission until someone calls it who should not have been able to.
func TestUnknownMethodsAreDenied(t *testing.T) {
	t.Parallel()

	policy := auth.Policy{"/pkg.v1.Svc/Known": {}}

	full := auth.Principal{Subject: "s", TenantID: "t", Scopes: []string{"everything"}}
	if err := policy.Check("/pkg.v1.Svc/BrandNew", full, true); err == nil {
		t.Fatal("a method with no policy entry was ALLOWED. Default-deny is the single " +
			"property this type exists to provide; without it every unlisted RPC ships open.")
	}
}

func TestCheckEnforcesScopesAndAuthentication(t *testing.T) {
	t.Parallel()

	policy := auth.Policy{
		"/pkg.v1.Svc/Public":  {Public: true},
		"/pkg.v1.Svc/AnyUser": {},
		"/pkg.v1.Svc/Read":    {Scopes: []string{"x:read"}},
		"/pkg.v1.Svc/Both":    {Scopes: []string{"x:read", "x:write"}},
	}

	reader := auth.Principal{Subject: "s", TenantID: "t", Scopes: []string{"x:read"}}
	writer := auth.Principal{Subject: "s", TenantID: "t", Scopes: []string{"x:read", "x:write"}}
	nobody := auth.Principal{}

	cases := []struct {
		name          string
		method        string
		principal     auth.Principal
		authenticated bool
		wantAllowed   bool
	}{
		{"public method needs nothing", "/pkg.v1.Svc/Public", nobody, false, true},
		{"unauthenticated is denied on a normal method", "/pkg.v1.Svc/AnyUser", nobody, false, false},
		{"authenticated with no scopes passes a no-scope rule", "/pkg.v1.Svc/AnyUser", nobody, true, true},
		{"correct scope is allowed", "/pkg.v1.Svc/Read", reader, true, true},
		{"missing scope is denied", "/pkg.v1.Svc/Read", auth.Principal{Scopes: []string{"x:write"}}, true, false},

		// The "all, not any" rule. A caller holding one of two required scopes must be
		// denied -- if this ever flipped to "any", every multi-scope rule would silently
		// weaken to its most permissive member, and the code would read identically.
		{"holding one of two required scopes is denied", "/pkg.v1.Svc/Both", reader, true, false},
		{"holding both required scopes is allowed", "/pkg.v1.Svc/Both", writer, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := policy.Check(tc.method, tc.principal, tc.authenticated)
			if gotAllowed := err == nil; gotAllowed != tc.wantAllowed {
				t.Errorf("allowed = %v, want %v (err: %v)", gotAllowed, tc.wantAllowed, err)
			}
		})
	}
}

// TestValidateCoverageReportsBothDirections is the mechanism behind the startup check that
// stops the server booting with an incomplete policy.
func TestValidateCoverageReportsBothDirections(t *testing.T) {
	t.Parallel()

	policy := auth.Policy{
		"/pkg.v1.Svc/Kept":    {},
		"/pkg.v1.Svc/Renamed": {}, // left behind after a rename
	}
	served := []string{"/pkg.v1.Svc/Kept", "/pkg.v1.Svc/Added"}

	// Nothing extra is declared, so "Renamed" exists nowhere and is genuinely stale.
	err := policy.ValidateCoverage(served, nil)
	if err == nil {
		t.Fatal("coverage passed despite a served method with no rule and a rule for nothing served")
	}

	// A served method with no rule would be denied at runtime -- the failure that must
	// never reach production.
	if !strings.Contains(err.Error(), "/pkg.v1.Svc/Added") {
		t.Errorf("the uncovered method is not named: %v", err)
	}
	// A rule for a method nobody serves reads in review as though it still protects
	// something. It does not.
	if !strings.Contains(err.Error(), "/pkg.v1.Svc/Renamed") {
		t.Errorf("the stale rule is not named: %v", err)
	}
	// The message has to say what to do. An error that only states a fact makes the reader
	// go looking for the file; this one names it.
	if !strings.Contains(err.Error(), "policy.go") {
		t.Errorf("the error does not say where to fix it: %v", err)
	}
}

func TestValidateCoveragePassesWhenTheyMatch(t *testing.T) {
	t.Parallel()

	policy := auth.Policy{"/pkg.v1.Svc/A": {}, "/pkg.v1.Svc/B": {Public: true}}
	if err := policy.ValidateCoverage([]string{"/pkg.v1.Svc/A", "/pkg.v1.Svc/B"}, nil); err != nil {
		t.Errorf("an exactly-matching policy was rejected: %v", err)
	}
}

// TestARuleForADeclaredButUnservedRPCIsAllowed is the escape hatch that makes the two
// coverage directions satisfiable at once.
//
// Before the `declared` argument existed, an RPC present in a .proto but not registered on
// this server was trapped: policy_coverage_test.go demanded a rule for it, and adding that
// rule made ValidateCoverage call the rule stale and refuse to boot the server. There was no
// state that satisfied both -- the only ways out were to delete the rule (recreating the
// silent gap the test exists to close) or to stop checking its whole package.
//
// A guard with no way to comply is a guard somebody eventually deletes, which is why this
// is a test and not a comment.
func TestARuleForADeclaredButUnservedRPCIsAllowed(t *testing.T) {
	t.Parallel()

	policy := auth.Policy{
		"/pkg.v1.Svc/Served":   {},
		"/pkg.v1.Svc/Declared": {Scopes: []string{"x:write"}}, // generated, not yet wired up
	}

	err := policy.ValidateCoverage(
		[]string{"/pkg.v1.Svc/Served"},
		[]string{"/pkg.v1.Svc/Served", "/pkg.v1.Svc/Declared"},
	)
	if err != nil {
		t.Errorf("a rule for a declared-but-unregistered RPC was rejected as stale: %v\n\n"+
			"Deciding the auth posture of an RPC BEFORE wiring it up is the desirable order. "+
			"Refusing it forces the decision to be deleted and re-made later, or not at all.", err)
	}

	// But a rule for a method in neither set is still stale -- the rename case must keep
	// failing, or this exemption would swallow the check whole.
	err = policy.ValidateCoverage(
		[]string{"/pkg.v1.Svc/Served"},
		[]string{"/pkg.v1.Svc/Served"},
	)
	if err == nil {
		t.Error("a rule for a method that is neither served nor declared was accepted; the " +
			"declared-set exemption has swallowed the stale-rule check entirely")
	}
}

// MethodsInPackages is deliberately NOT tested here.
//
// A meaningful test needs real descriptors in protoregistry.GlobalFiles, and this package's
// test binary imports no generated code -- the first version of that test failed with "no
// methods found for order.v1", correctly reporting that it would otherwise have been
// vacuously true. It lives in internal/grpcapi/policy_coverage_test.go instead, where the
// generated packages are genuinely linked in and the assertion means something.
