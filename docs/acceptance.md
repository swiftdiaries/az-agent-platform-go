# Acceptance record

This is the acceptance record for the conversation-agent platform described in [design.md](design.md). Each row is a fixture case, its status and the exact Go test that proves it. The staged plan and the progress ratchet are in [migration.md](migration.md).

- **PASS** means a Go test in this repository proves the case against fixtures: real Chat HTTP, PostgreSQL 16, controllable `internal/mcptest` endpoints, a scripted model or the real `litellm` provider over HTTP against `internal/modeltest`'s scripted Responses server.
- **BLOCKED** means no test can prove the case until an input from outside the platform exists (see [Integration inputs](design.md#integration-inputs)). The row names that input and the closest fixture test, which proves only the platform side of the seam.
- **Manual** means the check is a command, not a Go test.

A fixture PASS is not evidence about real Keycloak, Foundry, LiteLLM, the Java MCP server, a cost gateway or Kubernetes.

Test paths are relative to the repository root. `TestE2EConversationAgent/Sn` is the subtest `t.Run("Sn", ...)` in `./integration`, gated by the ratchet `e2eReachedStage` in `integration/e2e_progress_test.go` (currently 9, so S0 to S9 all run).

## Running the record

Every PASS row needs PostgreSQL 16. `internal/pgtest` starts a `postgres:16` container, or uses the isolated database named by `TEST_DATABASE_URL`. A missing database reports BLOCKED, never a pass.

```sh
go test -race -count=1 ./...                                  # everything
go test -race -count=1 ./integration -run 'TestE2EConversationAgent'   # S0..S9
```

CI runs the same suite on every push and pull request (see [CI and deploy](#ci-and-deploy)).

## End-to-end stages

One black-box test, `TestE2EConversationAgent` in `integration/e2e_test.go`: real Chat HTTP, three in-process replicas, PostgreSQL 16, two MCP endpoints (vN and vN+1), two tenants whose remote tool names differ, a scripted model.

| Stage | What it proves | Status | Test |
| --- | --- | --- | --- |
| S0 | An AG-UI POST on one replica reaches MCP and returns an answer | PASS | `TestE2EConversationAgent/S0` (`./integration`) |
| S1 | A sentinel credential reaches MCP but never appears in a database text or jsonb column, events, `slog` output or spans | PASS | `TestE2EConversationAgent/S1` (`./integration`) |
| S2 | Tenant A resolves to vN and tenant B to vN+1; a directory entry naming a non-allowlisted endpoint ends `dependency_unavailable` with zero requests to that endpoint | PASS | `TestE2EConversationAgent/S2` (`./integration`) |
| S3 | A paginated snapshot is captured at the first message; both tenants see the same aliases while MCP receives each tenant's own remote names (including `/` and a space); a second run revalidates without `tools/list` | PASS | `TestE2EConversationAgent/S3` (`./integration`) |
| S4 | `activate_journey` swaps in only that journey's aliases; sibling calls are not executed; per-tenant limits 2 and 1 evict the LRU journey; a user setting narrows a limit; a lowered limit evicts at the next run start; a pinned journey is never evicted; the active set and history survive restarting all replicas | PASS | `TestE2EConversationAgent/S4` (`./integration`) |
| S5 | A correction admitted through replica B while A's tool is blocked reaches every later provider request; a reconnect on C is gap-free | PASS | `TestE2EConversationAgent/S5` (`./integration`) |
| S6 | An approval wait, a tenant flip to vN+1, approve: superseded and a new interaction, approve again: dispatched exactly once on vN+1; a clarification reply survives the same flip | PASS | `TestE2EConversationAgent/S6` (`./integration`) |
| S7 | The real `litellm` provider against the scripted enforcement stub: attribution headers on every model request without a credential; 429 `budget_exceeded` ends `limit_exceeded` with `Retry-After`, no retry; a scripted `Admit` denial sends zero model requests; `agent_usage` rows match the stub's tokens | PASS | `TestE2EConversationAgent/S7` (`./integration`) |
| S8 | Design corrections: one alias across tenants and a version flip (M4), history truncation (M5), idle definition re-pin (M6), dispatch keeps a journey active (M9), 403 as a tool result (M15) | PASS | `TestE2EConversationAgent/S8` (`./integration`) |
| S9 | An apply-then-drop-response effect ends `outcome_unknown` with no retry and a pinned journey; the trace carries no content, tool descriptions or remote names | PASS | `TestE2EConversationAgent/S9` (`./integration`) |

## Credentials and tenancy

| Case | What it proves | Status | Test |
| --- | --- | --- | --- |
| Credential non-leakage | A credential never enters a database column, event, log or span; the type redacts under `fmt`, JSON and `slog` | PASS | `TestE2EConversationAgent/S1`; `TestRedactFmtVerbs`, `TestRedactJSON`, `TestRedactSlog` (`./internal/credential`); `TestRedactAccessFormatting`, `TestRedactAccessJSON`, `TestRedactAccessSlog` (`./internal/cost`) |
| Credential forwarding | Only the configured header names are copied and applied; a redirect does not forward them | PASS | `TestFromRequestCopiesOnlyNamed` (`./internal/credential`); `TestToolBindingDoesNotForwardCredentialsAcrossRedirect` (`./integration`) |
| Fresh credentials on resume | An approval reply supplies fresh credentials; none is stored | PASS | `TestApprovalFreshCredentialsAndRejectedReplies` (`./integration`) |
| Allowlist fail-closed | A non-allowlisted endpoint ends the run `dependency_unavailable` before any request is sent | PASS | `TestE2EConversationAgent/S2` (`./integration`); `TestOpenRejectsUnallowlistedEndpoint` (`./internal/catalog`); `TestAllowlistEndpoint` (`./internal/tenantdir`) |
| Run scope recorded | Directory, endpoint and settings are recorded on the run; a resolution failure is `dependency_unavailable` | PASS | `TestRunScopeRecordedAndDependencyUnavailable` (`./integration`) |
| Missing credentials | A request without credentials is rejected | PASS | `TestAuthRejectsMissingCredentials` (`./integration`) |
| Tenant directory API | The real directory service supplies tenant to product version and endpoint ID | BLOCKED: tenant directory API (static YAML adapter only). Closest: `TestE2EConversationAgent/S2` |
| Settings API | A real settings service supplies tenant and user layers | BLOCKED: settings API (static file adapter only). Closest: `TestLayering`, `TestMergeUserAboveTenantClamped` (`./internal/settings`) |
| Keycloak | A real Keycloak-signed JWT is accepted, and expiry ends `auth_required` | BLOCKED: configured Keycloak. Closest: `TestKeycloakAuthenticatorRefreshesForRotationAndBoundsUnknownKids` (`./internal/chat`) |

## Catalog and tool selection

| Case | What it proves | Status | Test |
| --- | --- | --- | --- |
| Paginated snapshots | A multi-page `tools/list` is captured, pinned per conversation, and committed with the pin | PASS | `TestE2EConversationAgent/S3` (`./integration`); `TestOpenCapturesPaginatedSnapshot` (`./internal/catalog`); `TestSnapshotCommitAndPin`, `TestSnapshotPinIsPerConversation` (`./internal/journal`) |
| Revalidation | A run revalidates the snapshot without re-listing when version, server version and endpoint are unchanged, and the revalidation refreshes the validation time, so the TTL counts from the last validation | PASS | `TestOpenRevalidatesWithoutList` (`./internal/catalog`); `TestRecordRunCatalogRefreshesValidation` (`./internal/journal`); `TestE2EConversationAgent/S3` (`./integration`) |
| Alias stability | One alias for a `[k.v2, k]` selector across tenants and across a version flip | PASS | `TestE2EConversationAgent/S8` (`./integration`); `TestOpenResolvesAliasesAcrossTenantsAlternatives` (`./internal/catalog`); `TestCompileSelectorAlias` (`./internal/definitions`) |
| Unavailable journeys | A journey whose selector does not resolve is hidden, never fatal mid-turn; an unknown or unavailable explicit target fails the run before any model call, and Chat reports it as `journey_unavailable` (or `policy_conflict` when a policy conflict made it unavailable), never `internal_error` | PASS | `TestE2EConversationAgent/S4` (`./integration`); `TestCodeUnknownTargetIsJourneyUnavailable`, `TestCodePolicyConflictTarget` (`./internal/runtime`); `TestCodeFailureReason` (`./internal/platform`); `TestCodeFailureCode` (`./internal/chat`); `TestCodeFinishFailedRecordsReason` (`./internal/journal`); `TestHarnessUnavailableJourneysAreHiddenNotFatal` (`./internal/runtime`); `TestOpenUnavailableJourney` (`./internal/catalog`); `TestJourneyUnavailableRunFailsWithoutModelCall` (`./integration`) |
| `policy_conflict` | One operation key or alias with different policies in two journeys: a static compile error, and a bind-time unavailable journey for tag matches | PASS | `TestCompilePolicyConflict` (`./internal/definitions`); `TestCheckPoliciesApprovalConflict`, `TestCheckPoliciesAliasConflict` (`./internal/catalog`); `TestOpenPolicyConflict` (`./internal/catalog`) |
| Tag-only matches are effectful | A tag match adds approval but never a class | PASS | `TestResolveTagApprovalOrsIn` (`./internal/catalog`) |
| Real Java `_meta` contract | The Java server publishes the operation key under `catalog.operation_meta_key` and `tags` under `_meta["tags"]` | BLOCKED: Java MCP `_meta` contract and key stability. Closest: `TestOpenResolvesAliasesAcrossTenants` (`./internal/catalog`) |

## Journeys, activation and the active set

| Case | What it proves | Status | Test |
| --- | --- | --- | --- |
| Activation and gating | After `activate_journey` the next round advertises only that journey's tools; a call to an inactive tool returns `{"error":"tool_not_active"}` and is never dispatched | PASS | `TestE2EConversationAgent/S4` (`./integration`); `TestHarnessActivationRebuildsAgentAndGatesTools` (`./internal/runtime`) |
| Sibling calls | Other calls in the activation's response are answered "not executed" | PASS | `TestDispatchPlanActivationMakesEverySiblingNotExecuted` (`./internal/runtime`) |
| Explicit target | A target journey is activated before round 0 | PASS | `TestHarnessExplicitTarget` (`./internal/runtime`) |
| Eviction under per-tenant limits | The limit comes from settings; activation beyond it evicts the LRU unpinned journey; lowering a limit evicts at the next run start | PASS | `TestE2EConversationAgent/S4` (`./integration`); `TestHarnessActivationEvictsLeastRecentlyUsed`, `TestHarnessLoweredLimitEvictsLeastRecentlyUsedAtRunStart` (`./internal/runtime`) |
| User narrows, never raises | A user value above the tenant's is clamped at resolve time | PASS | `TestMergeUserAboveTenantClamped`, `TestMergeUserNarrows`, `TestNarrowOnly` (`./internal/settings`) |
| True LRU | A journey's tool dispatch or successful `read_skill_resource` makes it most recent; `tool_not_active` and "not executed" calls touch nothing; the order is written once per round | PASS | `TestE2EConversationAgent/S8` (`./integration`); `TestLRUTouchAliasDispatchProtectsFromEviction`, `TestLRUTouchInactiveAndNotExecutedTouchNothing`, `TestLRUTouchPersistsOncePerRound`, `TestLRUTouchReadSkillResourceSuccessOnly` (`./internal/runtime`); `TestTouchThenActivateEvictsRightLRU`, `TestTouchNeverEvicts` (`./internal/journeys`) |
| Pinning | A pending or decided-undispatched approval, or an unresolved `dispatching` or `outcome_unknown` operation, pins its journey; denied, dispatched and superseded approvals and resolved operations do not | PASS | `TestActiveJourneysPinnedByPendingApproval`, `TestActiveJourneysPinnedByConsumedApproveUntilDispatched`, `TestActiveJourneysNotPinnedByDenyOrClarification` (`./internal/journal`); `TestPinUnresolvedOperationOutcomeUnknown`, `TestPinUnresolvedOperationDispatching`, `TestPinResolvedOperationDoesNotPin`, `TestPinSurvivesLoweredLimitAndEvictionPressure` (`./internal/journal`) |
| Pinned journeys are never evicted; activation is refused when all are pinned | `Activate` and `Fit` skip pinned journeys; the refusal is the model-visible result `activation_refused`; a refused explicit target ends the run with Chat code `activation_refused` | PASS | `TestActivateLeavesPinnedSliceAlone`, `TestFitThenActivateKeepsInvariant` (`./internal/journeys`); `TestTelemetryActivationRefusedEvent`, `TestCodeActivationRefusedTarget` (`./internal/runtime`); `TestCodeFailureReason` (`./internal/platform`); `TestCodeFailureCode` (`./internal/chat`) |
| Persistence across restart | The active set and history persist when all replicas restart | PASS | `TestE2EConversationAgent/S4` (`./integration`) |
| Skill bundles | Journey instructions come from `SKILL.md` bodies and the `conversation_prompt` base prompt; only selected material loads | PASS | `TestCompileConversationPrompt` (`./internal/definitions`); `TestSkillRuntimeLoadsOnlySelectedMaterial` (`./integration`); `TestComposeInstructionsListsAvailableAndActiveBodies` (`./internal/runtime`) |

## Steering

| Case | What it proves | Status | Test |
| --- | --- | --- | --- |
| Steering across replicas | A correction admitted on B while A's tool is blocked is included in every later provider request; reconnect on C is gap-free | PASS | `TestE2EConversationAgent/S5` (`./integration`); `TestSteeringBlockedToolAcrossThreeReplicas` (`./integration`); `TestHarnessSteeringIsDrainedAndIncludedEveryRound` (`./internal/runtime`) |
| Admission versus completion | The accept/run race is closed in both commit orders | PASS | `TestAdmissionFinishBothCommitOrders`, `TestAdmissionFinishDispositionAtomic` (`./integration`) |
| Reconnect cursor | A reconnect with a cursor replays the gap-free event tail | PASS | `TestReplayHTTPReconnectCursor` (`./integration`) |
| Three-service HTTP path | Steering, reconnect and a forced drain through three service instances | PASS | `TestAcceptanceThreeServiceHTTPSteeringReconnectAndForcedDrain` (`./integration`) |

## Approvals and clarification

| Case | What it proves | Status | Test |
| --- | --- | --- | --- |
| Markers | requested at Wait, decided at Reply (no network I/O), dispatched at the guard, denied, superseded | PASS | `TestApprovalMarkersRequestedAtWait`, `TestApprovalMarkersDecidedThenDispatched`, `TestApprovalMarkersDenied`, `TestApprovalMarkersDeniedOnExpiry`, `TestApprovalMarkersSupersede` (`./internal/journal`) |
| Expiry | An expired wait is a denial: the run ends `failed` with reason `interaction_expired`, Chat reports RUN_ERROR `interaction_expired` on reconnect, and a late reply gets 409 `interaction_expired` | PASS | `TestE2EConversationAgent/S6` (`./integration`); `TestExpiredInteractionReason`, `TestExpiredReplyReturnsErrExpired` (`./internal/journal`); `TestExpiredReplyStatus`, `TestCodeFailureCode` (`./internal/chat`) |
| Wait needs a recorded run scope | `Wait` is refused until `RecordRunScope` has run | PASS | `TestApprovalMarkersWaitRequiresRecordedScope` (`./internal/journal`) |
| Supersede on a product-version flip | Approve after the flip: superseded, a new interaction, dispatched once on vN+1 | PASS | `TestE2EConversationAgent/S6` (`./integration`); `TestApprovalMarkersSupersededOnDrift` (`./internal/journal`) |
| Journey-bound approvals | An approval is consumed only under the journey it was proposed in; if that journey is unavailable the model sees `approval_superseded` | PASS | `TestApprovalGuardJourneyBound` (`./internal/runtime`); `TestHarnessApprovalOwnedByJourneyAndOperationRecordsIt` (`./internal/runtime`); `TestApprovalSupersededWhenJourneyUnavailable` (`./integration`) |
| Binding mismatch | A changed binding supersedes and opens a new wait; a matching binding dispatches exactly once | PASS | `TestApprovalGuardBindingMismatchOpensNewWait`, `TestApprovalGuardMatchDispatchesOnce` (`./internal/runtime`) |
| Definition-set change | A decided approval is superseded when the pinned set changes | PASS | `TestApprovalSupersededOnDefinitionSetChange` (`./integration`) |
| Continuation keeps its pin | A reply continuation keeps the conversation's definition pin | PASS | `TestApprovalKeepsPinOnReplyContinuation` (`./integration`) |
| Clarification survives flips | A clarification reply has no marker, no journey binding, and survives a version flip | PASS | `TestE2EConversationAgent/S6` (`./integration`); `TestApprovalMarkersClarificationHasNoMarker` (`./internal/journal`); `TestHarnessClarificationHasNoJourney` (`./internal/runtime`) |
| Business repair needs new approval | Changed arguments after a rejection are a new proposed action | PASS | `TestBusinessErrorChangedActionRequiresNewApproval` (`./integration`) |
| Durable wait and reply | Clarification and approval waits survive pod replacement and resume once | PASS | `TestAcceptanceThreeServiceHTTPApprovalAndReconnect` (`./integration`) |

## Errors and outcomes

| Case | What it proves | Status | Test |
| --- | --- | --- | --- |
| `outcome_unknown` | A lost-after-effect call is not retried, the run ends, the journey is pinned, and the thread-level stop is kept | PASS | `TestE2EConversationAgent/S9` (`./integration`); `TestToolCallLostAfterEffectNoRetry` (`./integration`); `TestPinUnresolvedOperationOutcomeUnknown` (`./internal/journal`) |
| Database loss after dispatch | Owner database loss records unknown and interrupted | PASS | `TestToolCallOwnerDatabaseLossRecordsUnknownAndInterrupted` (`./integration`) |
| Safe-read retry | A `read_only` or `idempotent` call retries with a stable call ID | PASS | `TestToolCallVerifiedRetryStableID` (`./integration`) |
| `not_permitted` | A 403 on `tools/call` is the model-visible result `{"error":"not_permitted"}`, one MCP call, the Binding stays usable; a 401 still ends `auth_required` | PASS | `TestE2EConversationAgent/S8` (`./integration`); `TestBindingCallNotPermitted` (`./internal/catalog`); `TestNotPermittedIsAResultNotAFailure`, `TestNotPermittedDoesNotSpendRepairBudget`, `TestNotPermittedAuthRequiredStillFailsRun` (`./internal/runtime`); `TestServerForbidden`, `TestServerForbiddenOtherToolsServed` (`./internal/mcptest`) |
| 403 at Catalog.Open | A 403 on `initialize` or `tools/list` stays `auth_required` | PASS | `TestOpenForbiddenInitialize`, `TestOpenForbiddenToolsList` (`./internal/catalog`) |
| `dependency_unavailable` | Directory, allowlist, endpoint or settings failure ends the run before any dispatch | PASS | `TestE2EConversationAgent/S2` (`./integration`); `TestRunScopeRecordedAndDependencyUnavailable` (`./integration`); `TestFinishDependencyUnavailable` (`./internal/journal`) |
| Real Java effect semantics | A real apply-then-drop-response against the Java monolith | BLOCKED: Java MCP transport and reconciliation path. Closest: `TestE2EConversationAgent/S9` (`./integration`) |

## Cost and usage

| Case | What it proves | Status | Test |
| --- | --- | --- | --- |
| `limit_exceeded` on 429 | A budget or rate 429 ends the run `limit_exceeded` with `Retry-After`; never retried in-run, never `outcome_unknown`; a reconnect keeps `retryAfterSeconds` | PASS | `TestE2EConversationAgent/S7` (`./integration`); `TestCodeSnapshotLimitDetail` (`./internal/journal`); `TestCostModelLimitExceeded` (`./integration`); `TestLimitBudgetExceeded`, `TestLimitRateLimited` (`./internal/provider/litellm`) |
| LiteLLM HTTP 400 `budget_exceeded` | A 400 carrying the budget signal is a limit; a plain 400 is not | PASS | `TestLimit400BudgetExceeded`, `TestLimit400PlainIsNotLimit` (`./internal/provider/litellm`); `TestClassifyLimit400BudgetExceeded`, `TestClassifyLimit400Plain` (`./internal/cost`) |
| `Admit` denial | A denial ends the run `limit_exceeded` with zero model requests | PASS | `TestCostAdmitDeniedLimitExceeded` (`./integration`); `TestRunnerAdmit` (`./internal/runtime`) |
| Usage rows | One `agent_usage` row per successful model call, fenced, with attribution | PASS | `TestCostUsageRecordedPerModelCall` (`./integration`); `TestModelRunRecordsUsageOncePerSuccessfulCompletion` (`./internal/runtime`); `TestUsageRecordFenced` (`./internal/journal`); `TestUsageReport` (`./internal/cost`) |
| Attribution after Binding | `Admit` runs after Binding: a validated target is attributed, an untargeted run attributes an empty journey, an unknown target is rejected | PASS | `TestCostAttributionValidTarget`, `TestCostAttributionUntargeted`, `TestCostAttributionRejectsUnknownJourney` (`./integration`) |
| Attribution headers | The `Headers` gate sends the configured attribution headers and no credential | PASS | `TestE2EConversationAgent/S7` (`./integration`); `TestHeadersAdmitAllows` (`./internal/cost`) |
| Cost enforcement mechanism | A real enforcement point (Envoy or Linkerd RLS, LiteLLM virtual keys, agentgateway) counts and charges against budgets, accepting attribution only from Agent | BLOCKED: cost enforcement mechanism and gateway trust. Closest: `TestE2EConversationAgent/S7` (`./integration`) |
| Real LiteLLM and Foundry | Live provider calls with real Azure credentials | BLOCKED: real LiteLLM proxy and Foundry project. Closest: `TestCostModelLimitExceeded` (`./integration`) |

## History, definitions and rollout

| Case | What it proves | Status | Test |
| --- | --- | --- | --- |
| History budget | The oldest whole turns drop from the request only; calls keep their results; one `history_truncated` event with counts; persisted history stays full | PASS | `TestE2EConversationAgent/S8` (`./integration`); `TestTrimHistoryInvokeTrimsRequestKeepsPersisted`, `TestTrimHistoryInvokeOneEventPerTrimmingRequest` (`./internal/runtime`); `TestTrimHistoryNeverSplitsPairs`, `TestTrimHistoryKeepsProtectedTurn` (`./internal/runtime`); `TestHistoryTruncatedWritesEvent` (`./internal/journal`) |
| History budget config | The budget is required and must include the configured model | PASS | `TestHistoryBudgetSelectsConfiguredModelEntry`, `TestHistoryBudgetRejectsInvalidValuesWithoutEchoing` (`./cmd/agent-platform`) |
| Definition re-pin | An idle conversation re-pins to the current set at its next run and a `definition_changed` event is recorded; a pending approval, a reply continuation or an unresolved operation keeps its pin | PASS | `TestE2EConversationAgent/S8` (`./integration`); `TestRepinIdle`, `TestRepinKeepsPendingApproval`, `TestRepinKeepsReplyContinuation`, `TestRepinKeepsUnresolvedOperation` (`./internal/journal`); `TestVersionActivationRepinsIdleSessions` (`./integration`) |
| Bounded readiness | A pod is Ready for the current set plus sets referenced by open waits or unresolved operations; idle pins do not count | PASS | `TestConversationDefinitionsReady`, `TestConversationDefinitionsReadyIgnoresArchives`, `TestConversationActivateDefinition` (`./internal/journal`); `TestVersionActivationNonIdleSessionNeedsOldSet` (`./integration`) |
| Retained configs | Retained sets compile and serve the retained journey versions | PASS | `TestCompileRetain`, `TestCompileRetainCollisionAndLimits` (`./internal/definitions`); `TestVersionAwaitingSessionUsesRetainedProviderBundle` (`./integration`) |
| Strict definition schema | Unknown keys, `class:`/`alias:` on tag selectors, and a multi-alternative op without `alias:` are rejected | PASS | `TestCompileRejects`, `TestCompileRejectsRemovedKeys` (`./internal/definitions`) |

## Telemetry

| Case | What it proves | Status | Test |
| --- | --- | --- | --- |
| Spans carry opaque attributes only | `mcp.call` records the tool alias, operation key and outcome; the trace has no descriptions, remote names or content | PASS | `TestTelemetryMCPCallOutcome` (`./internal/catalog`); `TestTelemetryTraceHasNoDescriptionsOrRemoteNames` (`./integration`); `TestE2EConversationAgent/S9` (`./integration`) |
| Journey span events | `journey.activated`, `journey.activation_refused`, `journey.evicted` and `history.truncated` carry config IDs and counts | PASS | `TestTelemetryActivationEvent`, `TestTelemetryActivationRefusedEvent`, `TestTelemetryFitEvictionEvent`, `TestTelemetryHistoryTruncatedEvent`, `TestTelemetryTargetActivationEventAndLimitAttribute` (`./internal/runtime`) |
| Redaction processor | `Redact` drops `gen_ai.tool.description` from span and event attributes and passes everything else through | PASS | `TestRedactDropsToolDescription`, `TestRedactPassesThroughOtherwise`, `TestRedactDelegatesShutdownAndFlush` (`./internal/telemetry`) |
| Trace correlation without content | The MCP call carries the call ID and W3C trace context, with no content | PASS | `TestTelemetryMCPTraceAndCallIDAreCorrelatedWithoutContent`, `TestTelemetryInitializerInstallsW3CPropagation` (`./integration`); `TestExporterPostsToConfiguredTracePath` (`./internal/telemetry`) |
| Distributed trace into Java | One trace spans Go and the real Java server | BLOCKED: Java MCP transport trace propagation. Closest: `TestTelemetryMCPTraceAndCallIDAreCorrelatedWithoutContent` (`./integration`) |

## CI and deploy

| Case | What it proves | Status | Test |
| --- | --- | --- | --- |
| CI workflow | `.github/workflows/test.yaml` builds, vets, checks `gofmt` and runs `go test -race -count=1 ./...` with a `postgres:16` service on every push and pull request | Manual | The workflow file exists at `.github/workflows/test.yaml` (not a Go test; it runs the whole suite above) |
| Compose config | The Compose files render | Manual | `docker compose --env-file deploy/config/local.env -f deploy/compose.yaml -f deploy/compose.local.yaml config -q` (not a Go test; see [deploy/README.md](../deploy/README.md)) |
| Graceful drain | A draining service rejects new admission and interrupts an expired owner | PASS | `TestServiceDrainRejectsNewAdmissionAndInterruptsExpiredOwner` (`./integration`) |
| Bounded shutdown | A blocked admission or claim is bounded by the shutdown grace | PASS | `TestServiceShutdownBoundsBlockedAdmissionAndClaim` (`./integration`) |
| Config validation | Required variables and invalid values fail startup without echoing values | PASS | `TestLoadConfigBuildsExplicitKeycloakMapping`, `TestLoadConfigSelectsLiteLLMWithoutFoundryCredentialConfiguration` (`./cmd/agent-platform`) |
| Kubernetes rollout | Three ready pods, ConfigMap and Secret references, workload identity | BLOCKED: no configured Kubernetes environment; no manifest has been applied. Closest: `TestServiceDrainRejectsNewAdmissionAndInterruptsExpiredOwner` (`./integration`) |
