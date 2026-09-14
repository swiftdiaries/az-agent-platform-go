# Task 6A Keycloak ingress authentication

`chat.NewKeycloakAuthenticator` is the production implementation of
`chat.Authenticator`. It validates a signed Keycloak JWT before Chat creates a
conversation principal. It does not accept a caller-supplied user, tenant, or
decoded JWT payload.

The following is a new nonsecret configuration contract. Deployment supplies
these keys through a ConfigMap with `envFrom`; the process composition layer
maps them to `chat.KeycloakConfig`.

| ConfigMap environment key | `KeycloakConfig` field | Required meaning |
| --- | --- | --- |
| `KEYCLOAK_JWT_ISSUER` | `Issuer` | Exact HTTPS issuer expected in `iss`. |
| `KEYCLOAK_JWKS_URL` | `JWKSURL` | Exact HTTPS JWKS endpoint. |
| `KEYCLOAK_JWT_AUDIENCE` | `Audience` | Exact required `aud` value. |
| `KEYCLOAK_JWT_SOURCE` | `JWTSource` | `authorization_bearer` or `header`. |
| `KEYCLOAK_JWT_HEADER` | `JWTHeader` | One inbound header holding the JWT. |
| `KEYCLOAK_USER_CLAIM_PATH` | `UserClaimPath` | RFC 6901 JSON Pointer to a nonempty user string. |
| `KEYCLOAK_TENANT_CLAIM_PATH` | `TenantClaimPath` | RFC 6901 JSON Pointer to a nonempty tenant string. |
| `KEYCLOAK_JWT_ALLOWED_ALGORITHMS` | `AllowedAlgorithms` | Comma-separated supported RSA methods: `RS256`, `RS384`, or `RS512`. |

`authorization_bearer` requires exactly one configured header in the form
`Bearer <JWT>`. `header` requires exactly one configured header whose entire
value is the JWT. Repeated headers, comma-joined values, unsigned JWTs,
unconfigured algorithms, invalid issuer/audience/expiry, malformed claims,
and duplicate JSON claim names are unauthorized.

The adapter fetches HTTPS JWKS without credentials, refuses redirects, limits
the response and key count, times out the request, caches keys briefly, and
rate-limits unknown-key refreshes. A new `kid` can refresh the key set for a
normal Keycloak rotation; random unknown IDs cannot trigger unbounded JWKS
traffic. It never logs token, claim, tenant, or user values. The resulting
principal is an unambiguous length-delimited tenant/user value used only inside
the existing Chat-to-Platform seam.

Session cookies are not identity input for this adapter. They remain
request-scoped credentials: an MCP server receives `Cookie` only when its
existing `forward_headers` allowlist names it. Chat, Platform, persistence,
events, traces, definitions, and shared MCP clients do not retain them.

Actual issuer, audience, header, JWT source, claim paths, and allowed signing
algorithms are deployment values and have not been supplied. The configured
Keycloak acceptance cell therefore remains **BLOCKED** until those values and
the real service boundary are exercised.
