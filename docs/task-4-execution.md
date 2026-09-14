# Task 4 implementation candidate

Base: `6d2e684eb7b81adc0236e8ec301ab1a0a1f20a86` (reviewed Tasks 1–3). Scope is Task 4 only. Independent specification and quality reviews are pending.

The selected branch is the approved product-owned resume-in-place fallback. The pinned MAF graph's single-request round trip passes, but its two-outstanding-request resume calls the provider once with only one result and then again, producing duplicate output. Both findings are executable in `internal/runtime/framework_contract_test.go`; the negative gate asserts the observed limitation and therefore passes as regression evidence. Production keeps the existing MAF single-provider-turn seam and durable PostgreSQL authority.

Real-fixture evidence: PostgreSQL 16.13, Go 1.26.0, stateful MCP Go SDK v1.7.0. The suite starts an isolated database per test and fails rather than skips when PostgreSQL/Docker is unavailable. The process test ends one OS process after wait commit and a second after reply-admission commit, then explicitly resumes the one admitted continuation using fresh ingress.

Validation commands:

```sh
go test ./... -count=1
go test -race ./internal/... ./integration -run 'TestHITL|TestReply|TestToolCall|TestApproval|TestBusinessError|TestOwnership|TestSteering|TestAdmissionFinish' -count=1
go vet ./...
git diff --check
```

The full scenario mapping and exact framework failure are in `docs/superpowers/plans/2026-09-14-agent-platform/source-evidence.md`. Task 5 and Task 6 remain unimplemented here. No live Java/provider/cluster acceptance is claimed.

Final checks: full suite PASS (integration 18.986s; runtime 1.157s), focused race selection PASS, `go vet ./...` PASS, and `git diff --check` PASS. The implementation commit remains a review candidate, not an independent-review approval.

## Specification-review repair candidate

Repair base: `3b1a0a2cfa6378b68a13b0f94af96f350daf88c8` (candidate plus repair brief).
The submitted revision is the commit containing this entry. It repairs all five
review cards: exact epoch-zero reply-continuation recovery after reaper execution,
the attempt-two predecessor matrix, durable owner-loss operation uncertainty,
post-lock interaction TTL validation, and Chat-owned safe interaction projection.
Task 5 and Task 6 remain untouched. Independent specification and quality
rereviews are pending on the exact submitted revision.

Red evidence:

- `TestHITLSeparateProcessAfterWaitAndReplyCommit`: reaper changed the never-owned
  continuation to `interrupted`; after preserving that row, Claim still left the
  retransmission pending until the exact consumed-reply exception was added.
- `TestBeginAttemptSecondAttemptPredecessorMatrix`: completed, dispatching, and
  auth-required read-only/deduplicated rows reopened.
- `TestToolCallOwnerDatabaseLossRecordsUnknownAndInterrupted`: snapshot ended in
  `tool.started`, `run.interrupted`, with no durable outcome event or reconnect
  projection.
- `TestReplyWaitTransactionsAndTTL`: a reply whose TTL elapsed behind its row
  lock was admitted.
- `TestReplyChatProjectionAndOutstandingOrdering`: initial/live reconnect data
  exposed the provider call sentinel and `providerCallId`.

Final verification used the Docker PostgreSQL
`16.13 (Debian 16.13-1.pgdg13+1)` fixture with no skips:

```text
go test -race ./internal/... ./integration -run 'TestHITL|TestReply|TestToolCall' -count=1 -timeout=90s
PASS: internal/runtime 1.718s; integration 10.540s

go test -race ./internal/... ./integration -run 'TestJourney|TestAuth|TestToolBinding|TestPersistence|TestReplay|TestPostgres|TestAdmission|TestOwnership|TestSteering|TestAdmissionFinish' -count=1 -timeout=90s
PASS: integration 17.276s

go test ./... -count=1 -timeout=90s
PASS: internal/runtime 0.656s; integration 23.188s

go vet ./...
PASS

git diff --check
PASS
```

The fixture cleanup check found no remaining `agent-platform-task2-*` container.
The submitted changed paths are this report and repair brief,
`internal/chat/http.go`, `internal/journal/{interactions,observe,operations,owners,store}.go`,
and `integration/{interactions,outcomes,ownership}_test.go`.

### Card 1 specification-rereview repair

Specification rereview passed Cards 2–5 and found one remaining Card 1 race on
`78f2d14e4234b5e57a067d114a4f78578943f2a1`: Reap scanned expired run A without
a lock, then its thread-only locked helper could interrupt a newer expired
epoch-zero continuation C after the conversation advanced. The submitted repair
revision is the commit containing this entry; independent specification and
quality rereviews remain pending on that exact revision.

`TestReapStaleCandidateCannotInterruptNewContinuation` deterministically holds
the conversation lock after Reap scans A, replaces A with C, then releases the
stale reaper. Before the repair C became `interrupted`. The shared locked helper
now checks the exact Reap candidate and the pending/epoch-zero/consumed-
continuation exemption. The same test calls direct Admit and proves it preserves
C while attaching steering, after which the exact reply command claims C.
Existing checks prove ordinary expired, epoch-positive pending, and interrupted
runs remain ineligible for recovery.

Fresh final verification used PostgreSQL
`16.13 (Debian 16.13-1.pgdg13+1)` with no skips:

```text
go test -race ./internal/... ./integration -run 'TestHITL|TestReply|TestToolCall' -count=1 -timeout=90s
PASS: internal/runtime 1.691s; integration 11.378s

go test -race ./internal/... ./integration -run 'TestJourney|TestAuth|TestToolBinding|TestPersistence|TestReplay|TestPostgres|TestAdmission|TestOwnership|TestSteering|TestAdmissionFinish' -count=1 -timeout=90s
PASS: integration 18.769s

go test ./... -count=1 -timeout=90s
PASS: internal/runtime 0.976s; integration 25.742s

go vet ./...
PASS

git diff --check
PASS
```

The follow-up changes only this evidence, the repair brief,
`internal/journal/{commands,owners}.go`, and `integration/ownership_test.go`.
