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
