# Security

## This template does not currently authenticate anyone

Until milestone M5 lands, the only auth implementation is
`internal/platform/interceptor/devauth.go`, which **injects a fixed principal without
verifying anything**. Every request is treated as `dev-tenant` with full scopes.

Do not expose a build from this branch to any network you do not control.

Three things make that hard to do by accident, and all three are tested:

| Guard | Where |
|---|---|
| `APP_ENV=production` + `AUTH_MODE=dev` is refused before any listener binds | `config.Validate` |
| `AUTH_MODE=oidc` is refused outright, because the verifier does not exist yet | `config.Validate` |
| An unknown `AUTH_MODE` errors instead of falling back to a permissive default | `grpcapi.authInterceptors` |

That second guard exists because of a real bug, not a hypothetical one. `AUTH_MODE=oidc`
previously *validated and booted* while the server ignored it entirely and installed the dev
interceptor anyway — so the one value a reader sets **because** they read this warning was
the value that silently disabled the warning's own mitigation. It now fails closed.

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
- Missing hardening for milestones not yet built (M4–M11). The README status table is the
  authoritative statement of what exists.
