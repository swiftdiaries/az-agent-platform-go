# Agent platform implementation tasks

Read [index.md](index.md), [interfaces.md](interfaces.md) and the approved spec before implementing. Paths below are relative to the standalone destination repository. Each deliverable owns its tests and adds only the types it actually uses. The file list names starting locations; record exact assigned paths before work begins.

For each deliverable: write its behavioral tests, run the focused command and observe failure, implement, rerun successfully, then obtain independent review and integrate under [coordination.md](coordination.md). The coordinator records evidence in the index. Keep later implementation details open until earlier code exists.

## Task 1: One working authenticated journey

**Depends on:** none. **Scope:** `go.mod`, `go.sum`, `cmd/agent-platform/main.go`, `internal/platform/platform.go`, `internal/chat/http.go`, `internal/chat/auth.go`, `internal/runtime/run.go`, `internal/runtime/router.go`, `internal/definitions/load.go`, `internal/mcp/client.go`, `configs/journeys.yaml`, `configs/prompts/planner.md`, `integration/journey_test.go`.

- [ ] Bootstrap the standalone module with the provisional fork pin from source-evidence.md; verify resolution/checksums and record Go/module versions. Evaluate the real HTTP/MCP path against the spec’s framework gate; if required lifecycle interception cannot be demonstrated, use a product-owned harness behind the same Chat–Agent seam. Final selection awaits the durable wait/effect proofs in Task 4.
- [ ] Build the real AG-UI HTTP boundary with validated auth and a narrow Agent call. Route explicit and inferred targets through the same journey lookup; reject unknown targets and forged principals.
- [ ] Load one strict journey declaration, prompt and exact tool allowlist. Reject unknown fields, duplicate IDs, escaping paths and missing resources. Keep schemas on MCP. Use a controllable provider and MCP server for deterministic tests.
- [ ] Discover/bind tools using cloned, allowlisted per-run credentials. Prove empty/missing tools and normalization collisions fail closed; two principals cannot share credential state. Keep MCP sessions alive until that run finishes.
- [ ] Run a read-only reference journey end to end. Return sanitized auth/internal failures, stable call IDs and privacy-safe spans. This increment must not expose effectful tools before Task 4 supplies their guards.
- [ ] Verify `go test -race ./internal/... ./integration -run 'TestJourney|TestAuth|TestToolBinding' -count=1`: real HTTP input reaches only the selected tools, streams a visible answer and exposes no raw error or credential. Record independent Chat/Agent review.

**Produces:** a working HTTP-to-journey path using concrete internal code. **Acceptance:** initial coverage for spec cases 1, 6, 7 and 12; final coverage includes later durability/integration work.

## Task 2: Persistent conversations and reconnect

**Depends on:** 1. **Scope:** `internal/journal/store.go`, `internal/journal/migrate.go`, `internal/journal/migrations/000001_conversations.sql`, `internal/journal/commands.go`, `internal/journal/sessions.go`, `internal/journal/observe.go`, `internal/chat/observe.go`, `internal/runtime/run.go`, `integration/persistence_test.go`, `integration/replay_test.go`, `integration/postgres_test.go`.

- [ ] Add isolated PostgreSQL 16 test databases with cleanup and diagnostics, and a migration runner. Reuse one fixture for all later durability tests; verify two databases with identical IDs remain isolated.
- [ ] Persist conversation/journey identity, pinned definition digest, provider history JSON, commands, runs and sequenced events; framework session objects remain private and reconstructable. Validate schema constraints and repeatable migration on fresh databases.
- [ ] Atomically deduplicate command/event admission by communication ID; identical duplicate submissions return the original receipt, changed payloads conflict. Save provider history, authoritative Agent run state and completion events atomically at completion.
- [ ] Resume a completed journey after process replacement without a client transcript. Verify isolation across conversations/principals and immutable session definition binding.
- [ ] Detach run lifetime from HTTP connection lifetime. Read snapshot and watermark from one MVCC view; poll events after the greater of cursor/watermark, using notifications only as hints. Render snapshots directly from Agent records.
- [ ] Verify `go test -race ./internal/... ./integration -run 'TestPersistence|TestReplay|TestPostgres|TestAdmission' -count=1`: duplicate admission, disconnect survival, restart, cursor below/equal/above watermark, writes during snapshot and lost notifications. Assert sequence-based client deduplication, not exactly-once network delivery.

**Produces:** durable idle resume and live event replay; ownership remains a development-only single-process constraint until Task 3. **Acceptance:** case 2 persistence and case 3 reconnect.

## Task 3: Any-replica ownership and steering

**Depends on:** 2. **Scope:** `internal/journal/owners.go`, `internal/journal/runs.go`, `internal/journal/commands.go`, `internal/runtime/controller.go`, `internal/runtime/inbox.go`, `integration/ownership_test.go`, `integration/steering_test.go` and the next lifecycle migration.

- [ ] Claim only explicit pending run IDs; renew owner epoch/lease and fence every owned mutation. Reap an expired run as interrupted; never transfer or automatically resume that run. Use database time checked after row locks.
- [ ] Check ownership before each model/MCP dispatch and after tool completion. Renewal failure cancels local work; stale writes fail. Do not claim remote calls were canceled.
- [ ] Materialize the explicit handoff, selected conversation context, retained journey history and ordered pending commands at the provider boundary. Record inclusion only when directly observed in that request.
- [ ] Serialize admission and finish with one conversation lock. The controller calls the store's finish operation directly. Keep follow-up credentials only in the live owner's memory; after owner loss require fresh authorized ingress instead of storing/forwarding credentials through PostgreSQL.
- [ ] Verify `go test -race ./internal/... ./integration -run 'TestOwnership|TestSteering|TestAdmissionFinish' -count=1`: use barriers for both admission/finish commit orders, a blocked tool on A, steering on B and reconnect on C. Include lease expiry during lock wait, failed inclusion recording and owner loss. Prove no orphaned live-owner command, stale commit or automatic takeover.

**Produces:** one authoritative execution owner and durable safe-point steering. **Acceptance:** case 3; interruption/fencing portions of cases 4 and 5.

## Task 4: Durable human input and safe tool outcomes

**Depends on:** 3. **Scope:** `internal/journal/interactions.go`, `internal/journal/operations.go`, `internal/runtime/interactions.go`, `internal/runtime/tool_policy.go`, `internal/chat/http.go`, `internal/mcp/client.go`, `integration/interactions_test.go`, `integration/outcomes_test.go` and the next interaction/operation migration.

- [ ] Atomically enter a human wait: save provider history, pending call IDs, pinned definition, interaction, event and awaiting state, then release ownership. Resume with fresh authenticated credentials after replacing the process.
- [ ] Atomically validate/consume the answer and admit one continuation. Validate the complete question/answer shape, principal, expiry and interaction kind. Inject crashes before/after both wait and reply commits; duplicates must not create another continuation or replay completed tools.
- [ ] Persist the authoritative proposed call and lossless deterministic argument binding. Approval is exact and one-time; changed arguments ask again. Deny, timeout, forged call, wrong principal/kind and custom text never dispatch the protected tool.
- [ ] Use serial guarded autocall. Retain MAF only if its qualified seams pass the durable wait/effect product-slice proofs; otherwise use the product-owned harness behind the existing seam. Record stable logical operation/call IDs and distinct retry attempts. Bound safe retries under one deadline/budget; disable or account for lower-layer retries and stop retries after partial visible output when replay would duplicate it.
- [ ] Classify read-only, idempotent and effectful calls from reviewed integration policy. An ambiguous effect records operation outcome `outcome_unknown` and stops execution without sibling dispatch, model re-entry or automatic retry. Owner loss separately marks the run `interrupted`; assert both records when an effect is uncertain. A later run preserves the unresolved uncertainty until explicit reconciliation/product policy permits action.
- [ ] Distinguish MCP isError business rejection from transport uncertainty, auth and internal failure. Permit one safe argument repair within the budget; changed approved actions require approval again. Never call a provider or MCP server inside a database transaction callback.
- [ ] Verify `go test -race ./internal/... ./integration -run 'TestInteraction|TestApproval|TestOutcome|TestRetry|TestBusinessError' -count=1`: separate-process wait/reply, duplicate/conflicting input, applied-write/lost-response, owner database loss while MCP remains reachable, safe-read retry, partial-stream failure and sanitized error/auth cases.

**Produces:** durable clarification/approval and conservative effect handling. **Acceptance:** cases 4, 6 and 8–12.

## Task 5: Pinned definitions, bounded skills and second journey

**Depends on:** 4. **Scope:** `internal/definitions/load.go`, `internal/definitions/registry.go`, `internal/skills/source.go`, `internal/runtime/router.go`, `internal/runtime/run.go`, `configs/journeys.yaml`, `configs/prompts/shift-swap.md`, `configs/skills/planner/SKILL.md`, `configs/skills/shift-swap/SKILL.md`, `integration/definitions_test.go`, `integration/skills_test.go` and definition-current storage.

- [ ] Complete startup compilation and immutable digest indexing. Hash normalized declaration, prompt and allowed skill files independent of absolute paths; reject mutation, missing resources and conflicting digest content. Retain every referenced bundle.
- [ ] Use the database current-version pointer only for new journey instances. Pods must have retained and candidate bundles before readiness; activate the deployment version only once the new replicas are ready. No runtime bundle activation or copy-on-write registry API.
- [ ] Expose skill metadata first and selected instructions/supporting files on demand. Reuse the native source where it satisfies the contract; add only the containment/pinning adapter it lacks. Reject traversal, symlinks, undeclared files and post-compilation mutation. Expose no script execution tool.
- [ ] Configure shift swapping as the second reference journey without platform API/storage changes. Prove inferred/explicit routing, selected context and tool/skill isolation across both journeys.
- [ ] Verify `go test -race ./internal/... ./integration -run 'TestDefinition|TestSkill|TestSecondJourney' -count=1`: metadata reads no bodies, both versions remain usable after restart, missing pinned bundles prevent readiness and failed rollout leaves the current pointer unchanged.

**Produces:** configuration-only journey expansion and immutable retained bundles. **Acceptance:** completes cases 1 and 2; definition portion of case 5.

## Task 6: Deployment and complete acceptance

**Depends on:** 5. **Scope:** `deploy/compose.yaml`, `deploy/kubernetes.yaml`, `internal/telemetry/telemetry.go`, `cmd/agent-platform/main.go`, `integration/deployment_test.go`, `integration/acceptance_test.go`, `docs/acceptance.md`.

- [ ] Deploy at least three replicas and PostgreSQL. On shutdown fail readiness, stop claims and let owned work finish within grace; forced termination leads to interruption, never handoff. Verify additive definition activation and pod replacement.
- [ ] Exercise actual configured auth, model provider and Java MCP transport/header mapping. Preserve fixture results separately; missing credentials/services/cluster block only the dependent checks and cannot satisfy release acceptance.
- [ ] Verify one Go trace across Chat, hub, journey, provider and MCP plus Java call-ID correlation. Claim a distributed trace only if the integration observes both sides. Assert no prompts, user text, skill bodies, tool payloads, credentials or raw business identifiers under production defaults.
- [ ] Run `go test -race ./... -count=1` in the destination with isolated PostgreSQL and the required integration environment. Record the exact revision, toolchain/dependency/database versions, commands/results, Kubernetes evidence and independent review in docs/acceptance.md.
- [ ] Complete all twelve numbered spec cases. Every case must have evidence; any missing required real-environment check leaves V1 incomplete.

**Produces:** the tested V1 service and one acceptance report. **Acceptance:** cases 1–12, including graceful drain, forced loss and real Java trace correlation.

## Acceptance coverage

| Spec case | Deliverables |
|---|---|
| 1: route, declared tools/skills, user headers | 1, 5, 6 |
| 2: second configuration-only journey, pinned restart | 2, 5, 6 |
| 3: steering race and reconnect across replicas | 2, 3, 6 |
| 4: database loss during external effect | 3, 4, 6 |
| 5: additive definitions, readiness and graceful/forced stop | 3, 5, 6 |
| 6: expired auth, no durable/shared credentials | 1, 4, 6 |
| 7: trace correlation and privacy | 1, 6 |
| 8: durable questions and exact approvals | 4, 6 |
| 9: bounded safe-read retries | 4, 6 |
| 10: uncertain effect stops execution | 4, 6 |
| 11: safe business repair, renewed approval | 4, 6 |
| 12: auth versus sanitized internal failure | 1, 4, 6 |
