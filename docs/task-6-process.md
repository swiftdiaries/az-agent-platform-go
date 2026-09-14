# Task 6 process contract

## Lifecycle

`/livez` returns success while the process is serving. It never opens PostgreSQL,
Foundry, or MCP connections. `/readyz` returns success only when the process is
not draining and PostgreSQL can verify every current or session-referenced
definition digest against the loaded immutable registry. It never calls a model
or MCP server.

On `SIGTERM`, the command entrypoint will call `Service.Shutdown` with
`AGENT_PLATFORM_SHUTDOWN_GRACE`. The service first fails readiness and rejects
new command admission. Admission and the initial PostgreSQL owner claim share
the drain lock: a command accepted before the boundary has already been claimed;
a command after it is rejected without a journal write. The service lets owned
work finish until the grace deadline, then cancels local work and closes
long-lived HTTP streams. It does not take over, replay, or hand off interrupted
runs. Lease expiry remains the existing journal reaper's interruption-only path.

## Nonsecret ConfigMap

The deployment ConfigMap is the authoritative configuration source. It supplies
these nonsecret values through `envFrom`; actual values have not been supplied.

| Key | Required value |
| --- | --- |
| `AGENT_PLATFORM_PORT` | HTTP listen port |
| `AGENT_PLATFORM_SHUTDOWN_GRACE` | Go duration for graceful drain |
| `AGENT_PLATFORM_CONFIG` | mounted immutable `journeys.yaml` path |
| `AGENT_PLATFORM_RETAINED_CONFIGS` | comma-separated mounted retained bundle paths when database pins require them |
| `KEYCLOAK_JWT_ISSUER` | Keycloak issuer |
| `KEYCLOAK_JWKS_URL` | HTTPS JWKS endpoint |
| `KEYCLOAK_JWT_AUDIENCE` | expected audience |
| `KEYCLOAK_JWT_SOURCE` | `authorization_bearer` or `header` |
| `KEYCLOAK_JWT_HEADER` | header name when source is `header` |
| `KEYCLOAK_USER_CLAIM_PATH` | RFC 6901 user-claim path |
| `KEYCLOAK_TENANT_CLAIM_PATH` | RFC 6901 tenant-claim path |
| `KEYCLOAK_JWT_ALLOWED_ALGORITHMS` | comma-separated RSA JWT algorithms |
| `AZ_AGENT_FOUNDRY_PROJECT_ENDPOINT` | Azure AI Foundry project endpoint |
| `AZ_AGENT_FOUNDRY_DEPLOYMENT` | Foundry deployment name |

Every listed ConfigMap key is required except
`AGENT_PLATFORM_RETAINED_CONFIGS`, which is optional and empty when no session
pin needs an older bundle. There are no implicit file, port, or grace-period
defaults: `AGENT_PLATFORM_PORT` must be an integer from 1 through 65535 and
`AGENT_PLATFORM_SHUTDOWN_GRACE` must be a positive Go duration. Missing or
invalid values stop startup with the fixed message `agent platform startup
failed`; values and Secret details are not printed.

`journeys.yaml` remains mounted ConfigMap content. It declares the Java MCP
endpoint and the exact forwarded-header allowlist; deployment does not add an
ad hoc endpoint override or interpolate secret values into YAML. Header values
are request-scoped credentials only. They are not written to the journal,
events, traces, logs, ConfigMap, or this document.

`DATABASE_URL` is a required process environment variable supplied with a
Kubernetes Secret reference, never by the ConfigMap. AAD credentials belong in
Kubernetes workload identity or explicit Secret references. Secret values are
never committed or logged. Missing Keycloak,
Foundry, Java MCP, or cluster values leave their real-boundary acceptance cells
`BLOCKED`; fixture coverage does not change that state.

Foundry is limited to an AAD-authenticated Azure AI Foundry **project endpoint**
and deployment through the Responses API. Generic Foundry endpoints and API-key
authentication are unsupported. The process constructs an Azure default AAD
credential, so workload identity is the expected production path. The Foundry
adapter refuses redirects before a redirected request can receive that bearer
credential.

## Delivery split

Task 6 began from `5c5d0e29d6e68b51cbc1ea89905ff3e83afceb62` in parallel:

- A: Keycloak authentication (Terra)
- B: Foundry provider adapter (Terra)
- C: process lifecycle and eventual service wiring (Terra)
- D: telemetry (Luna), once a worker slot is available
- E: container and deployment manifests (Luna), after this document's probe and
  configuration contract
- F: combined three-replica, twelve-case acceptance (Terra), after A through E

Each slice receives focused independent review before integration. The command
entrypoint composes A and B through `service.Dependencies`. No real acceptance
claim is made until supplied
configuration permits the corresponding checks.
