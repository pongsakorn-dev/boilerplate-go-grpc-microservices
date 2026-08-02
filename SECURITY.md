# Security

## Authentication exists, and the default still authenticates nobody

`AUTH_MODE=oidc` is a real JWT/JWKS verifier. `AUTH_MODE=dev` — the **default** — injects a
fixed principal without verifying anything, so that a fresh clone runs with no identity
provider. Every request is `dev-tenant` with full scopes.

Do not expose a `dev`-mode build to any network you do not control.

Four guards make that hard to do by accident, and all four are tested:

| Guard | Where | Test |
|---|---|---|
| `APP_ENV=production` + `AUTH_MODE=dev` is refused before any listener binds | `config.Validate` | `config_test.go::TestValidateRejectsDevAuthInProduction` |
| An unknown `AUTH_MODE` errors instead of falling back to something permissive | `auth.NewVerifier` | `verifier_test.go::TestNewVerifierRefusesAnUnknownMode` |
| `AUTH_MODE=oidc` with no audience is refused — an empty audience accepts every token the issuer ever minted | `config.Validate`, `NewOIDCVerifier` | `config_test.go::TestValidateRefusesHalfConfiguredOIDC` |
| An RPC with no authorisation rule stops the server booting | `Policy.ValidateCoverage`, called by `NewServer` | `policy_coverage_test.go` |

The second guard exists because of a real bug rather than a hypothetical one. The server
previously installed the dev interceptor unconditionally and never read `AUTH_MODE`, so
`AUTH_MODE=oidc` *validated and booted* while the setting was ignored entirely — the one
value a reader sets **because** they read this warning was the value that silently disabled
the warning's own mitigation. It was confirmed by calling an RPC with no credentials and
getting three orders back.

## What the OIDC verifier rejects

`alg:none`; HS256 MAC'd with the issuer's public key; symmetric (`oct`) keys in the JWKS;
RSA below 2048 bits; an algorithm the JWKS did not publish for that key; wrong `aud`; wrong
`iss`; unknown `kid`; missing `exp`; a missing tenant claim; and a discovery document whose
`issuer` does not match the configured one. Each is a case in
`internal/platform/auth/oidc_verifier_test.go`, run against an in-process hostile issuer.

One honest note on that suite: the HS256-with-the-public-key case passes even with both of
our own algorithm checks deleted, because the key cache refuses symmetric keys outright and
golang-jwt rejects a typed public key for HMAC before our checks run. That was established by
deleting them. It is kept as a regression test, and
`TestKeyAlgorithmBindingIsEnforced` covers the substitution our code alone catches.

## Fixed during M5's review pass

An adversarial review of the auth code found four defects that were confirmed by
reproduction. They are recorded because a template's value is partly in what it shows you
about how these go wrong.

| Defect | Consequence | Fix |
|---|---|---|
| `Recovery` returned an unmapped `*apperr.Error` while sitting **outermost**, so nothing mapped it | grpc-go fell back to `codes.Unknown` + `err.Error()`, sending the client the **raw panic value** — a panic on a DSN leaked the password. Present since M3 | `Recovery` now maps its own error; `recovery_test.go::TestRecoveryThroughTheRealChain` drives the real chain instead of calling `ToStatus` by hand |
| JWKS cache held its write lock across the HTTP fetch | One **unauthenticated** request naming an unknown `kid` stalled every concurrent verification for the IdP's full response time, ignoring deadlines, while holding admission slots | Fetches serialise on a separate mutex; readers never wait on I/O |
| Cached key set had no maximum age | An issuer revoking a key had **no effect**: legitimate tokens name the still-cached keys, so no miss ever triggered a refetch and the revoked key stayed trusted until restart | `OIDC_MAX_KEY_AGE`, default 15m |
| Discovery failure memoised by `sync.Once` | A pod whose first request arrived during an IdP restart served `Unauthenticated` **forever** — and liveness reports process health by design, so nothing restarted it | Only success is memoised; retries are rate-limited |

Three tests were also found to be **vacuous** — passing for reasons other than the ones they
named. The algorithm-confusion test passed with both of our own defences deleted (golang-jwt
rejects it on key type first), and the discovery issuer-mismatch test passed on a 404 without
ever reaching the check. Each is now either fixed or annotated with what it actually proves.

## Supported versions

**None.** This is a template, not a library. There are no security releases and no backports.

You are expected to fork it, and the fork is yours: once you copy this code, its
vulnerabilities are your vulnerabilities. Watch this repository if you want to hear about
issues found after your fork point, and rebase deliberately.

## Reporting a vulnerability

Use GitHub's **private vulnerability reporting** on this repository
(Security → Report a vulnerability). Please do not open a public issue for anything
exploitable.

Because this is a template, the useful report is usually not "this service can be attacked"
— it is **"a fork that followed this template would be vulnerable, and here is why the
template led them there."** A pattern that reads as correct and is not is the highest-value
finding here.

Expect an acknowledgement within a week. There is no bounty.

## What is in scope

- A default, or a documented pattern, that is unsafe in a fork built from it
- A guard that claims to prevent something and does not
- Anything that fails **open**: a misconfiguration that results in more access rather than a
  refusal to start

## What is out of scope

- The `dev` auth mode accepting any request. That is its documented purpose; the finding
  would be a *route to enabling it in production* that the guards above do not catch.
- Vulnerabilities in dependencies with no exploitable path here. Report those upstream;
  `govulncheck` runs in CI.
- Missing hardening for milestones not yet built (M4, M6–M11). The README status table is the
  authoritative statement of what exists.
