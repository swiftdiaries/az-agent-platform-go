# V1 acceptance ledger

Status: **incomplete**. This ledger is the Task 6 evidence map, not a release
claim. The final candidate source is
`94e9a63ab608149408da7cb25a6219f1033ff579`; Task 5's integrated completion is
`5c5d0e2`. No configured Keycloak, Foundry, Java MCP, or Kubernetes environment
was provided, so those real-boundary cells are blocked.

## Configuration boundary

The Kubernetes ConfigMap supplies only nonsecret values with `envFrom`:

| Keys |
| --- |
| `AGENT_PLATFORM_PORT`, `AGENT_PLATFORM_SHUTDOWN_GRACE`, `AGENT_PLATFORM_CONFIG`, `AGENT_PLATFORM_RETAINED_CONFIGS` |
| `KEYCLOAK_JWT_ISSUER`, `KEYCLOAK_JWKS_URL`, `KEYCLOAK_JWT_AUDIENCE`, `KEYCLOAK_JWT_SOURCE`, `KEYCLOAK_JWT_HEADER`, `KEYCLOAK_USER_CLAIM_PATH`, `KEYCLOAK_TENANT_CLAIM_PATH`, `KEYCLOAK_JWT_ALLOWED_ALGORITHMS` |
| `AZ_AGENT_FOUNDRY_PROJECT_ENDPOINT`, `AZ_AGENT_FOUNDRY_DEPLOYMENT` |
| `OTEL_SERVICE_NAME`, `OTEL_SERVICE_VERSION`, optional `OTEL_EXPORTER_OTLP_ENDPOINT` (the full OTLP HTTP traces URL, including `/v1/traces`) |

`DATABASE_URL` comes from a Kubernetes Secret reference. AAD credentials come
from workload identity or explicit Secret references. Optional
`OTEL_EXPORTER_OTLP_HEADERS` also comes from a Secret reference. Compose requires
the explicit Secret-provided `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, and
`AZURE_CLIENT_SECRET`; it does not use a host Azure CLI credential. Kubernetes
uses workload identity. Neither values nor secret material belong in this
repository, logs, traces, this ledger, or journey YAML. The checked-in deployment
artifacts have not been applied.

## Deterministic evidence map

All entries below require PostgreSQL 16. The integration fixture starts an
isolated `postgres:16` container unless `TEST_DATABASE_URL` names an equivalent
isolated PostgreSQL 16 database. Fixture evidence is not real Keycloak,
Foundry, Java, or Kubernetes evidence.

| Spec case | Runnable fixture evidence | Current result |
| --- | --- | --- |
| 1. Route, declared tools/skills, allowed headers | `TestJourneyAuthenticatedAGUIToMCP`, `TestDeclarativeJourneyRouting`, `TestAcceptanceThreeServiceHTTPSteeringReconnectAndForcedDrain` | PASS fixture |
| 2. Second configured journey and pinned restart | `TestPersistenceDisconnectAndReplacement`, `TestVersionActivationPreservesPinnedSessions`, `TestJourneyDefinitionRoutesAndMaterializesContext` | PASS fixture |
| 3. Blocked-tool steering race and reconnect through three replicas | `TestAcceptanceThreeServiceHTTPSteeringReconnectAndForcedDrain`, `TestSteeringBlockedToolAcrossThreeReplicas`, `TestReplayHTTPReconnectCursor` | PASS fixture |
| 4. Database loss after external dispatch | `TestToolCallOwnerDatabaseLossRecordsUnknownAndInterrupted`, `TestOwnershipLossDuringModelRejectsResultAndRequiresFreshIngress` | PASS fixture; real Java effect is BLOCKED |
| 5. Additive definitions, readiness, graceful drain, forced interruption | `TestVersionActivationPreservesPinnedSessions`, `TestServiceDrainRejectsNewAdmissionAndInterruptsExpiredOwner`, `TestAcceptanceThreeServiceHTTPSteeringReconnectAndForcedDrain` | PASS fixture; Kubernetes rollout is BLOCKED |
| 6. Expired credentials and no durable/shared credentials | `TestJourneyAuthenticatedAGUIToMCP`, `TestApprovalFreshCredentialsAndRejectedReplies` | PASS fixture; real Keycloak expiry is BLOCKED |
| 7. Correlated trace and privacy defaults | `TestJourneyAuthenticatedAGUIToMCP`, `TestExporterPostsToConfiguredTracePath` | PASS fixture; real Go-to-Java trace is BLOCKED |
| 8. Durable clarification and exact approval | `TestHITLDurableWaitReply`, `TestHITLCompletedToolNotReplayed`, `TestApprovalFreshCredentialsAndRejectedReplies`, `TestAcceptanceThreeServiceHTTPApprovalAndReconnect` | PASS fixture |
| 9. Bounded safe-read retries | `TestToolCallVerifiedRetryStableID` | PASS fixture |
| 10. Ambiguous effect stops execution | `TestToolCallLostAfterEffectNoRetry`, `TestToolCallOwnerDatabaseLossRecordsUnknownAndInterrupted` | PASS fixture; real Java effect is BLOCKED |
| 11. Business repair and renewed approval | `TestBusinessErrorChangedActionRequiresNewApproval` | PASS fixture |
| 12. Auth-required versus sanitized internal failure | `TestJourneyAuthenticatedAGUIToMCP`, `TestAuthRejectsMissingCredentials` | PASS fixture; real Keycloak boundary is BLOCKED |

The new service-level tests use three independently served `service.Service`
instances with shared PostgreSQL, real AG-UI HTTP requests, reconnect, durable
steering or approval, and a bounded drain/forced-loss path. Their model and MCP
peers are fixtures; they do not claim a Java trace or an external provider.

Local candidate evidence, run on 2026-09-15 from
`94e9a63ab608149408da7cb25a6219f1033ff579`:

| Check | Result |
| --- | --- |
| `go test -race ./... -count=1 -timeout=120s` | PASS; PostgreSQL `16.13 (Debian 16.13-1.pgdg13+1)` isolated fixture; integration package 32.431s |
| `go vet ./...` | PASS |
| `go version` | `go1.26.0 darwin/arm64` |
| `docker build -t az-agent-platform:task-6-candidate .` | PASS; image `sha256:289a79cdf1798ca3affba308fa32771fefb1723b85f381f52b8bdc07888256ee`, user `nonroot:nonroot`, entrypoint `/app/agent-platform` |
| `docker compose -f deploy/compose.yaml config --quiet` | PASS with synthetic nonsecret configuration |
| `kubectl config current-context` | BLOCKED: no current context is configured; no manifest was applied |

The fixture cases use three independently served `service.Service` instances
with shared PostgreSQL, real AG-UI HTTP requests, reconnect, durable steering
or approval, and a bounded drain/forced-loss path. Their model and MCP peers
are fixtures; the PASS entries do not claim a Java trace, an external provider,
or Kubernetes rollout.

## Required final commands and external evidence

Repeat after root integrates the candidate, from the exact Git revision recorded
with the result:

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
