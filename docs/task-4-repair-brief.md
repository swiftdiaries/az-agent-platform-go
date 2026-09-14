# Task 4 repair brief

Resume in `.worktrees/task-4` on `implement/task-4`. Candidate `8b3038d573fe28b7589570a866275c6691540248`
is based on reviewed Tasks 1–3 at `6d2e684eb7b81adc0236e8ec301ab1a0a1f20a86`.
Its checks passed on PostgreSQL 16.13, but specification review failed on the five defects below. No repair has been made; both reviews are pending.

## Fixed architecture choices

- PostgreSQL remains lifecycle authority. Chat owns external identity and safe
  projection; Agent journal owns runs, interactions, operations and events.
- Continue product-owned resume-in-place. The pinned MAF graph re-enters the provider once per result for two outstanding calls and duplicates output. Keep the executable contracts at
  `internal/runtime/framework_contract_test.go:16-121` and `:124-250`; see
  `docs/superpowers/plans/2026-09-14-agent-platform/source-evidence.md:79-88`.
- Preserve Task 1 credential/header/redirect privacy, Task 2 durable opaque IDs
  and replay, and Task 3 epochs, post-lock checks, no takeover, terminal command
  dispositions and external command correlation.
- Scope ends at Task 4. Leave Task 5 definitions/skills and Task 6 deployment untouched.

Use one implementation worker. Complete cards 1–5 in order. Cards 1 and 4
overlap interaction admission/tests; cards 3 and 5 overlap observation/HTTP.
Make each card red then green, then submit one revision for both reviews.

## Card 1: recover a committed continuation that never started

**Defect.** `internal/journal/interactions.go:297-313` consumes the reply and
inserts a pending continuation. Retransmission is recognized at `:251-261`, and
`internal/platform/platform.go:111-145` tries to launch it. After 15 seconds,
`Store.Claim` rejects it at `internal/journal/owners.go:47-60`.

**Invariant.** Fresh authenticated ingress may claim an explicitly named
pending continuation never owned (`owner_epoch = 0`), even after initial lease
expiry. Positive-epoch, previously owned, and interrupted runs are never taken
over. The duplicate reply creates no second command/run.

**Red check.** Extend `TestHITLSeparateProcessAfterWaitAndReplyCommit`
(`integration/interactions_test.go:218`): let the reply-admission child exit,
expire the continuation lease, retransmit through a fresh Platform, and require
one provider continuation. Pair it with a previously-owned/interrupted rejection
near `integration/ownership_test.go:132`. Observe both fail first.

**Files.** `internal/journal/owners.go`, `internal/platform/platform.go` only if
launch detection needs it, `integration/interactions_test.go`, and possibly
`integration/ownership_test.go`.

**Boundary/done.** Recovery is explicit ingress for the same command with fresh
credentials. Add no polling or takeover. Evidence must show epoch-zero recovery,
owned/interrupted rejection, one continuation row, and one model re-entry.

**Repair status.** Implemented. The reaper-first regression failed with the
continuation interrupted; after the scoped repair it preserves and reclaims only
the exact epoch-zero consumed-reply continuation. Owned/interrupted guards pass.
Specification rereview then found the unlocked reaper candidate could become
stale while waiting for the conversation lock. The locked helper now rechecks the
exact candidate and applies the same continuation exemption for Reap and Admit.

## Card 2: enforce the second-attempt predecessor matrix

**Defect.** `internal/journal/operations.go:50-58` accepts attempt 2 for
read-only/deduplicated operations regardless of prior outcome, reopening
`dispatching`, `completed`, and `auth_required` operations.

**Invariant.** Attempt 2 is allowed only after `rejected`, or after
`outcome_unknown` when stored/requested policy is `read_only` or `deduplicated`.
Reject other predecessors, effectful unknown, policy/binding changes, and attempt
3.

**Red check.** Add a table-driven direct store test beside
`integration/outcomes_test.go:159`. Set each predecessor/policy pair, call
`BeginAttempt(..., 2, ...)`, and assert the full allow/deny matrix plus unchanged
rows on denial. Observe bad predecessors succeed first.

**Files/boundary.** Change only `internal/journal/operations.go` and
`integration/outcomes_test.go`. Use a direct predicate; add no transition
framework and infer no safety from MCP annotations.

**Done.** Matrix, stable product-call ID, bounded retry, approval and business
error tests pass.

**Repair status.** Implemented. The matrix regression first reopened completed,
dispatching, and auth-required safe-policy rows; the direct predecessor predicate
now passes the full allow/deny matrix without mutating denied rows.

## Card 3: publish owner-loss operation uncertainty

**Defect.** `internal/journal/owners.go:166-175` marks unfinished attempts and
operations `outcome_unknown` but appends only `run.interrupted`.
`internal/journal/observe.go:20-68` has no stable operation outcome for snapshot
or reconnect.

**Invariant.** The same reaper transaction records one `tool.outcome_unknown`
per changed operation with stable product call ID and tool name, then the
distinct `run.interrupted`. Snapshot and reconnect expose both in order;
provider correlation IDs stay private.

**Red check.** Extend
`TestToolCallOwnerDatabaseLossRecordsUnknownAndInterrupted`
(`integration/outcomes_test.go:477`) to reconnect through Chat after reaping.
Require snapshot plus event stream to contain the product-call outcome followed
by run interruption and omit provider ID. Observe the outcome missing first.

**Files.** `internal/journal/owners.go`, `internal/journal/observe.go`,
`internal/journal/store.go` only if snapshot needs a field,
`internal/chat/http.go`, and `integration/outcomes_test.go`.

**Boundary/done.** Keep uncertainty distinct from interruption and append inside
the existing transaction. Claim no cancellation/retry/reconciliation. Database,
snapshot, and below/equal-watermark reconnect must agree atomically.

**Repair status.** Implemented. The regression first observed `tool.started`
followed directly by `run.interrupted`; database events, repeatable-read snapshot,
and cursor-zero/exact-watermark Chat reconnect now expose the safe ordered pair.
Quality rereview found the same missing event when failed `EndAttempt` persistence
falls through `Finish`. Both Finish and Reap now use one transaction-local helper
that records changed operations and their safe events before the run terminal.

## Card 4: recheck interaction TTL after the row lock

**Defect.** `internal/journal/interactions.go:269` evaluates expiry in the same
`SELECT ... FOR UPDATE`; PostgreSQL may evaluate it before lock wait.

**Invariant.** Lock the interaction row, then query/check database
`clock_timestamp()` while holding the lock. Expiry during lock wait rejects and
expires the reply atomically without a continuation.

**Red check.** Add a barrier to `TestReplyWaitTransactionsAndTTL`
(`integration/interactions_test.go:166`): hold the interaction lock, start reply
admission, let TTL pass, release, then require expired state, zero continuation
commands/runs, and expiry event/history closure. Observe stale validity admit it.

**Files/boundary.** Change `internal/journal/interactions.go` and
`integration/interactions_test.go`. Follow the post-lock database-clock pattern
at `internal/journal/owners.go:55-58` and `:73-94`; use no application or
transaction-start time.

**Done.** The barrier deterministically proves expiry during lock wait cannot
consume the interaction or create a continuation.

**Repair status.** Implemented. The row-lock regression first admitted an expired
reply; the post-lock database-clock check now expires it with zero continuation
commands or runs.

## Card 5: project a safe interaction at the Chat edge

**Defect.** Durable `ProposedCall` contains `providerCallId` at
`internal/journal/interactions.go:29-35`. Chat serializes it directly in snapshot
at `internal/chat/http.go:172` and `interaction.requested` at `:180-189`.

**Invariant.** Agent history retains provider correlation. Chat emits an
explicit safe shape with stable product `callId` and reviewable proposal fields,
never `providerCallId` or raw provider history. Initial and reconnect projection
match.

**Red check.** Extend `TestReplyChatProjectionAndOutstandingOrdering`
(`integration/interactions_test.go:296`) with visibly different product/provider
sentinels. Assert initial and reconnect payloads contain only product call ID and
no provider ID/history. Observe leakage first.

**Files/boundary.** Change `internal/chat/http.go` and
`integration/interactions_test.go`; keep journal artifact unchanged. Use the
smallest explicit Chat DTO/helper, preserving product call ID and exact approval
binding.

**Done.** Initial snapshot, live request event, and reconnect share the safe
shape; provider resume still passes.

**Repair status.** Implemented. The sentinel regression first exposed
`providerCallId`; Chat now projects one explicit action DTO for snapshot and live
events while the durable provider correlation remains available for resume.

## Final verification and handoff

Use actual PostgreSQL 16.13. Docker socket/network and Go cache require
escalated execution. Missing database prerequisites fail; no skips.

```sh
go test -race ./internal/... ./integration -run 'TestHITL|TestReply|TestToolCall' -count=1 -timeout=90s
go test ./... -count=1 -timeout=90s
go vet ./...
git diff --check
git diff --cached --check
```

Record each red test, final results, PostgreSQL version, fixture cleanup, exact
submitted SHA and changed paths in `docs/task-4-execution.md` or
`coordination.md`. Request fresh specification and quality review of that SHA.
Do not mark Task 4 reviewed or start Task 5/6.
