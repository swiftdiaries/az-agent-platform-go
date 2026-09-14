# Execution coordination

Use the delivery table in [index.md](index.md) as the only task tracker. No custom scheduler, JSON manifest or report archive is required.

1. The coordinator assigns one ready deliverable and records its owner, exact base commit, worktree and named writable paths in the table. Dependencies must have integrated, reviewed evidence.
2. Authors implement in isolated worktrees. Parallel work, when useful, must have disjoint writable paths; the coordinator owns integration and shared-file scheduling.
3. Use the least costly capable author; raise capability for durability/security decisions. The root coordinates and reviews; implementation belongs to assigned workers.
4. Write the focused behavioral test first, observe its failure, implement, and run the task's checks. Use real isolated PostgreSQL for transaction tests; absent external prerequisites are `BLOCKED`, not passing evidence.
5. Record the submitted commit, exact check commands/results and independent review in the table or a linked concise report. Review the exact submitted revision; cover both Chat and Agent for boundary changes.
6. Integrate one reviewed change at a time, stage only assigned files, run `git diff --check` and `git diff --cached --check`, and run affected checks on the integrated revision before releasing dependents.
7. Resume from the recorded commit and next unchecked step. Stop a stale worker before replacing it; preserve its changes, use a new worktree, and review any revised commit again. A stale branch is never evidence of completion.

Keep independent review and verification. Split a deliverable only when its implementation exposes a useful independent review boundary, not to meet a task-count quota.

## Task 1 submission

- Status: authored on `implement/task-1` from `a3c341302261333b5ce8b86d7a419ee3ce9aadf5`; submitted commit is the commit containing this entry. Independent specification and quality reviews remain pending.
- Contract clarification: Chat obtains the principal from its configured server-side authenticator. The stateful Streamable HTTP MCP run gets a fresh client session and forwards only registry-allowlisted headers, including the deployment-configured session cookie/OAuth headers. Raw headers are never stored in the process-local graph.
- Task 1 state boundary: thread/run/event state and communication-ID deduplication are process-local fixture state. PostgreSQL durability, restart/reconnect, and concurrent admission move to Task 2; Task 1 does not claim those acceptance results.
- Checks: `go test ./internal/... ./integration -run 'TestJourney|TestAuth|TestToolBinding' -count=1` passed (`integration 0.686s`); `go test ./... -count=1` passed (`integration 0.340s`); `go test -race ./internal/... ./integration -run 'TestJourney|TestAuth|TestToolBinding' -count=1` passed (`integration 1.729s`). `go version` reported `go1.26.0 darwin/arm64`; `go list -m -json` and `go mod download -json` resolved the planned MAF fork revision and exact recorded module/go.mod checksums.
- Changed paths: `go.mod`, `go.sum`, `cmd/agent-platform/main.go`, `configs/journeys.yaml`, `configs/prompts/planner.md`, `internal/chat/{auth,http}.go`, `internal/definitions/load.go`, `internal/mcp/client.go`, `internal/platform/platform.go`, `internal/runtime/{router,run}.go`, `integration/journey_test.go`, this file, and `source-evidence.md`.
- Next entry: root obtains independent Chat/Agent specification and quality reviews on the submitted revision. Task 2 starts only from the reviewed, integrated Task 1 revision.

### Task 1 specification-review repair

- The independent specification review failed commit `b1e7173c824063b773fe0aa4d3c9867617fbb852`. The follow-up commit containing this entry adds the four requested Task 1 repairs: startup catalog matching, typed downstream `auth_required`, the complete privacy-checked trace chain, and a product-owned call ID distinct from provider correlation. Rereview remains pending; no release gate is passed.
- Repair checks: `go test ./integration -run 'TestJourneyAuthenticated|TestJourneyConfig' -count=1` passed (`integration 0.809s`); focused trace and auth/outcome reruns passed (`integration 0.709s`, then `0.722s`); final `go test ./... -count=1` passed (`integration 0.389s`); final `go test -race ./internal/... ./integration -run 'TestJourney|TestAuth|TestToolBinding' -count=1` passed (`integration 1.821s`).

### Task 1 quality-review repair

- The independent quality review found that configured credentials followed MCP redirects and that raw client IDs crossed the Chat/Agent boundary. The follow-up commit containing this entry rejects redirects before any destination request and maps external thread/run identity to stable, principal-scoped, process-local opaque thread/run/communication IDs. AG-UI responses retain external correlation, while Agent state, call IDs, and trace attributes use only opaque IDs. Specification and quality rereviews remain pending.
- Repair checks: `go test ./integration -run 'TestJourneyAuthenticated|TestToolBindingDoesNotForward' -count=1` passed (`integration 0.746s`); `go test ./... -count=1` passed (`integration 0.735s`); `go test -race ./internal/... ./integration -run 'TestJourney|TestAuth|TestToolBinding' -count=1` passed (`integration 1.805s`); `go vet ./...` passed.

## Delivery status

| Deliverable | Status / owner / commit / checks / review |
|---|---|
| 1 | implemented and reviewed / `implement/task-1` / `11948213104a7bd2af6c2233f4d47cefd97ba874` / worker: `go test ./... -count=1`, required race suite and `go vet ./...` passed; specification reviewer `/root/task_1_spec_review`: required race suite passed (`integration 1.824s`); quality reviewer `/root/task_1_quality_review`: focused race suite passed, with zero redirect-target requests and ID-mapping/privacy assertions verified / both independent reviewers passed the exact commit with clean worktree and diff |
| 2 | ready / unassigned / starts from the Task 1 integration-evidence commit / persistence, replay and admission checks remain pending |

### Task 1 review history

- Specification review failed `b1e7173c824063b773fe0aa4d3c9867617fbb852`; `87732333f1774d0723521ff76b36c66b07063592` repaired startup catalog matching, typed downstream `auth_required`, trace coverage and product call identity.
- Quality review then found credential forwarding across redirects and raw external IDs crossing the Chat/Agent boundary; `11948213104a7bd2af6c2233f4d47cefd97ba874` rejected redirects and introduced principal-scoped opaque IDs.
- `/root/task_1_spec_review` and `/root/task_1_quality_review` independently passed `11948213104a7bd2af6c2233f4d47cefd97ba874`. Task 2 is ready from the integration-evidence commit containing this history.

## Task 2 submission

- Status: authored on `implement/task-2` from reviewed Task 1 base `c471498a33d0769aa716a0213ecd49acb7a76aed` in `.worktrees/task-2`; the submitted revision is the commit containing this entry. Independent specification/quality review remains pending; this entry passes no gate.
- Delivered: versioned/checksummed migrations; durable principal-scoped Chat identity mapping; Agent conversation, pinned journey/provider history, command/run/event records; serialized idempotent admission and one-live-run exclusion; atomic history/state/event completion; detached local execution; direct run-record snapshots and committed-event polling; authenticated GET reconnect with `Last-Event-ID`.
- Test-first evidence: admission/isolation tests first failed on the absent journal package; atomic-completion tests failed on absent `Start`/`Finish`; disconnect/replacement/replay tests failed against the prior synchronous platform interface. All now pass on isolated PostgreSQL 16.13. An intentional temporary `ReadCommitted` mutation made `TestReplaySnapshotConcurrentCommit` fail on an old watermark with new events; `RepeatableRead` was restored before final checks.
- Durability checks cover 12 concurrent identical admissions, changed duplicate conflicts, original receipt after completion, database/principal isolation, schema constraints/drift, immutable session digest, fresh runner/Chat/Platform reconstruction without client transcript, HTTP cancellation survival, credential-free Agent records, below/equal/ahead cursors, direct journal writes without notifications, and a writer committed while snapshot reading is blocked. Injected completion failure after history/event writes leaves the fingerprint of all Agent tables unchanged.
- Final checks: `go test ./... -count=1` passed (`integration 3.780s`); `go test -race ./internal/... ./integration -run 'TestPersistence|TestReplay|TestPostgres|TestAdmission' -count=1` passed (`integration 4.647s`); `go vet ./...` passed. `go mod tidy` completed; `git diff --check` passed.
- Limits: single-process development execution only; no Task 3 ownership, leases, epoch fencing, reclaim, or live steering. Interrupted/commit-failed runs remain live until that recovery layer is implemented. MCP stateful sessions, configured header forwarding, OAuth ingress checks, tool allowlists, no-tool response, and sanitized errors retain their Task 1 regression coverage.
- Next: independent reviewers inspect the exact submitted commit and both authority boundaries. Root records review/integration and only then releases Task 3.
