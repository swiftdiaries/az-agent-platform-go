# V1 acceptance ledger

Status: **incomplete**. This ledger is the Task 6 evidence map, not a release
claim. It is based on `334739438bd91ac9c9b91708b1d25d646c80c33a`; Task 5's
integrated completion is `5c5d0e2`. The final command must be rerun from the
root-selected integrated revision after the deployment and telemetry slices
land. No configured Keycloak, Foundry, Java MCP, or Kubernetes environment was
provided, so those real-boundary cells are blocked.

## Configuration boundary

The Kubernetes ConfigMap supplies only nonsecret values with `envFrom`:

| Keys |
| --- |
| `AGENT_PLATFORM_PORT`, `AGENT_PLATFORM_SHUTDOWN_GRACE`, `AGENT_PLATFORM_CONFIG`, `AGENT_PLATFORM_RETAINED_CONFIGS` |
| `KEYCLOAK_JWT_ISSUER`, `KEYCLOAK_JWKS_URL`, `KEYCLOAK_JWT_AUDIENCE`, `KEYCLOAK_JWT_SOURCE`, `KEYCLOAK_JWT_HEADER`, `KEYCLOAK_USER_CLAIM_PATH`, `KEYCLOAK_TENANT_CLAIM_PATH`, `KEYCLOAK_JWT_ALLOWED_ALGORITHMS` |
| `AZ_AGENT_FOUNDRY_PROJECT_ENDPOINT`, `AZ_AGENT_FOUNDRY_DEPLOYMENT` |

`DATABASE_URL` comes from a Kubernetes Secret reference. AAD credentials come
from workload identity or explicit Secret references. Neither values nor secret
material belong in this repository, logs, traces, this ledger, or journey YAML.
The deployment artifact is still pending its Task 6 deployment worker; do not
apply a substitute manifest.

## Deterministic evidence map

All entries below require PostgreSQL 16. The integration fixture starts an
isolated `postgres:16` container unless `TEST_DATABASE_URL` names an equivalent
isolated PostgreSQL 16 database. Fixture evidence is not real Keycloak,
Foundry, Java, or Kubernetes evidence.

| Spec case | Runnable fixture evidence | Current result |
| --- | --- | --- |
| 1. Route, declared tools/skills, allowed headers | `TestJourneyAuthenticatedAGUIToMCP`, `TestDeclarativeJourneyRouting`, `TestAcceptanceThreeServiceHTTPSteeringReconnectAndForcedDrain` | PENDING final integrated run |
| 2. Second configured journey and pinned restart | `TestPersistenceDisconnectAndReplacement`, `TestVersionActivationPreservesPinnedSessions`, `TestJourneyDefinitionRoutesAndMaterializesContext` | PENDING final integrated run |
| 3. Blocked-tool steering race and reconnect through three replicas | `TestAcceptanceThreeServiceHTTPSteeringReconnectAndForcedDrain`, `TestSteeringBlockedToolAcrossThreeReplicas`, `TestReplayHTTPReconnectCursor` | PENDING final integrated run |
| 4. Database loss after external dispatch | `TestToolCallOwnerDatabaseLossRecordsUnknownAndInterrupted`, `TestOwnershipLossDuringModelRejectsResultAndRequiresFreshIngress` | PENDING final integrated run; real Java effect is BLOCKED |
| 5. Additive definitions, readiness, graceful drain, forced interruption | `TestVersionActivationPreservesPinnedSessions`, `TestServiceDrainRejectsNewAdmissionAndInterruptsExpiredOwner`, `TestAcceptanceThreeServiceHTTPSteeringReconnectAndForcedDrain` | PENDING final integrated run; Kubernetes rollout is BLOCKED |
| 6. Expired credentials and no durable/shared credentials | `TestJourneyAuthenticatedAGUIToMCP`, `TestApprovalFreshCredentialsAndRejectedReplies` | PENDING final integrated run; real Keycloak expiry is BLOCKED |
| 7. Correlated trace and privacy defaults | `TestJourneyAuthenticatedAGUIToMCP` | PENDING telemetry integration; real Go-to-Java trace is BLOCKED |
| 8. Durable clarification and exact approval | `TestHITLDurableWaitReply`, `TestHITLCompletedToolNotReplayed`, `TestApprovalFreshCredentialsAndRejectedReplies`, `TestAcceptanceThreeServiceHTTPApprovalAndReconnect` | PENDING final integrated run |
| 9. Bounded safe-read retries | `TestToolCallVerifiedRetryStableID` | PENDING final integrated run |
| 10. Ambiguous effect stops execution | `TestToolCallLostAfterEffectNoRetry`, `TestToolCallOwnerDatabaseLossRecordsUnknownAndInterrupted` | PENDING final integrated run; real Java effect is BLOCKED |
| 11. Business repair and renewed approval | `TestBusinessErrorChangedActionRequiresNewApproval` | PENDING final integrated run |
| 12. Auth-required versus sanitized internal failure | `TestJourneyAuthenticatedAGUIToMCP`, `TestAuthRejectsMissingCredentials` | PENDING final integrated run; real Keycloak boundary is BLOCKED |

The new service-level tests use three independently served `service.Service`
instances with shared PostgreSQL, real AG-UI HTTP requests, reconnect, durable
steering or approval, and a bounded drain/forced-loss path. Their model and MCP
peers are fixtures; they do not claim a Java trace or an external provider.

Local development evidence for this task: `GOCACHE=$PWD/.cache/go-build go test
-race ./integration -run '^TestAcceptance' -count=1` passed on 2026-09-15 with
the isolated PostgreSQL `16.13 (Debian 16.13-1.pgdg13+1)` fixture. This is a
focused pre-integration result only; it does not change any `PENDING` or
`BLOCKED` final cell above.

## Required final commands and external evidence

Run after integration, from the exact Git revision recorded with the result:

```sh
GOCACHE=$PWD/.cache/go-build go test -race ./... -count=1
```

Record the Go version, `go list -m all`, PostgreSQL 16 version, command output,
and independent review with that revision. Then, only with supplied deployment
configuration and the final `deploy/kubernetes.yaml`, record separately:

1. Three ready Kubernetes pods, ConfigMap references for the listed nonsecret
   keys, and Secret/workload-identity references for `DATABASE_URL` and AAD.
2. A real Keycloak-signed ingress JWT and expiry result.
3. A real Foundry project-endpoint call using AAD.
4. A real Java MCP call showing the opaque tool-call ID and W3C trace context on
   both sides, while production spans exclude prompts, user text, skills, tool
   payloads, credentials, and raw business identifiers.

Until those four records exist, V1 remains incomplete regardless of fixture
results.
