# Keycloak: the worked OIDC example

`realm-export.json` configures a Keycloak realm that this service can actually verify tokens
from. It is a **development** realm — passwords are `alice`/`bob`, the client secret is
literally `dev-only-not-a-real-secret`, and `sslRequired` is off.

> [!NOTE]
> **Verified.** This realm has been imported by a real Keycloak 26.4 and a token from it
> accepted by the service. Measured end to end during M10:
>
> - `Realm 'gomicro' imported` — no errors
> - client-credentials token claims: `"aud":"orderd"`, `"tenant_id":"acme"`,
>   `"scope":"orders:read orders:write"`, `"token_use":"service"`
> - `orderd` with `AUTH_MODE=oidc`: **401** without a token, **200** with one, and the created
>   order carried `tenant_id: acme` straight from the claim
>
> Booting it also found a real bug — see *The two principal shapes* below.

## Why Keycloak, when the verifier is provider-agnostic

Nothing in `internal/platform/auth` knows what Keycloak is. The verifier speaks OIDC —
discovery, JWKS, `iss`/`aud`/`exp`, a configurable claim path — and the whole test suite runs
against an in-process issuer with no provider at all.

Keycloak is here because a template needs one worked example you can actually run, and
because it makes the audience trap concrete rather than theoretical.

## The audience trap

**A Keycloak access token's default `aud` is `account`, not your API.** Every integration
hits this, and the resulting `invalid audience` error sends people looking at their issuer
URL, their clock, and their keys — none of which are the problem.

The fix is an **audience protocol mapper** on each client that requests tokens for this API.
Both clients in the realm file have one:

```json
{
  "name": "audience-orderd",
  "protocolMapper": "oidc-audience-mapper",
  "config": { "included.client.audience": "orderd", "access.token.claim": "true" }
}
```

The `orderd` client itself is `bearerOnly` — nothing logs in as it. It exists purely so
other clients have an audience to name.

`OIDC_AUDIENCE` is refused when empty rather than defaulted, because a verifier with no
expected audience accepts every token the issuer ever minted, including tokens for unrelated
applications sharing the same Keycloak.

## The two principal shapes

| Client | Grant | `sub` | Tenant comes from | `Principal.IsService` |
|---|---|---|---|---|
| `orders-worker` | client credentials | `orders-worker` | a hardcoded-claim mapper (`acme`) | `true` |
| `orders-web` | authorization code + PKCE | the user's id | the user's `tenant_id` **attribute** | `false` |

`IsService` is derived from RFC 9068 §5 — in the client credentials grant there is no
resource owner, so `sub` equals `client_id`. `orders-web` users carry `azp: orders-web`,
which must **not** make them services; `claims_test.go` asserts exactly that.

Two users exist in different tenants (`alice`→`acme`, `bob`→`globex`) so cross-tenant
behaviour can be exercised by hand, mirroring
`grpcapi/auth_test.go::TestTenantIsolationIsDrivenByTheToken`.

## Running it

```bash
docker run --rm -p 8080:8080 -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e KC_BOOTSTRAP_ADMIN_PASSWORD=admin -v "%CD%/deploy/keycloak:/opt/keycloak/data/import" quay.io/keycloak/keycloak:26.4 start-dev --import-realm
```

Then point the service at it:

```bash
set AUTH_MODE=oidc && set OIDC_ISSUER_URL=http://localhost:8080/realms/gomicro && set OIDC_AUDIENCE=orderd && go run ./cmd/orderd
```

`http://` is accepted here only because the host is loopback — `auth/oidc.go` refuses
plaintext to any other host, since bearer tokens in cleartext are one wiretap from a full
compromise.

Get a machine token:

```bash
curl -d grant_type=client_credentials -d client_id=orders-worker -d client_secret=dev-only-not-a-real-secret http://localhost:8080/realms/gomicro/protocol/openid-connect/token
```

## Using a different provider

Change two environment variables, not any code:

| Provider | `OIDC_TENANT_CLAIM` | `OIDC_SCOPE_CLAIM` |
|---|---|---|
| Keycloak (this realm) | `tenant_id` | `scope` |
| Keycloak (realm roles) | `tenant_id` | `realm_access.roles` |
| Auth0 | `https://yourapp.example.com/tenant_id` | `scope` |
| Cognito | `custom:tenant_id` | `cognito:groups` |
| Entra ID | `tid` | `scp` |

Dotted paths walk nested objects; a literal claim name containing dots (Auth0's namespaced
claims) is matched first, so no escaping is needed. Every row above is covered by
`auth/claims_test.go::TestClaimMappingHandlesEveryProviderShape`.
