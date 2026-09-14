# MAF durable human-wait compatibility spike

## Verdict

**Qualified yes for the planned wait boundary.** Current Go MAF can support
durable exact-action approval and typed clarification with a small adapter,
while retaining the real `agent.Agent`, `agent.Session`, `toolautocall`, and
`mcptool` loop. It needs mandatory fail-closed autocall configuration and a
small transport-error classifier for effectful tools. Native defaults alone are
unsafe after an ambiguous effectful MCP transport failure.

This is an executable spike, not production platform code.

## Proven boundaries

- Native MAF `Session` JSON round-trips provider history and private autocall
  approval state. On restore, approval binding ignores a forged changed call ID,
  tool name, and arguments, and invokes the recorded original MCP call.
- A safe read completes once before the approval wait. A first OS process exits;
  a fresh test-binary process restores the MAF session from PostgreSQL and
  continues with the prior read, original write call, and matching result in the
  provider-visible protocol history.
- PostgreSQL `FOR UPDATE` admission binds a pending interaction to its principal
  and kind. Two fresh resume processes race; one claims and one receives
  `ErrAlreadyConsumed`. A third claim is also rejected.
- Denial, wrong principal, wrong interaction kind, duplicate reply, and changed
  client tool payload do not dispatch the protected or attacker-selected MCP
  action.
- Typed clarification uses a public non-invocable `tool.SchemaTool` named
  `request_clarification`. MAF surfaces and stores its structured function call,
  then stops autocall. The adapter persists question/options as kind `question`;
  a fresh process supplies a matching tool result containing either an option
  or custom answer. This cannot be consumed through the approval path.
- MCP calls cross a real local streamable-HTTP MCP transport. Credential A is
  observed only on the pre-wait read, and fresh credential B only on the resumed
  write. Neither value appears in the durable pending/session JSON inspected
  both before and after resume.
- The unguarded counterexample applies an effect, drops the HTTP response, then
  observes native autocall dispatch a later serial sibling and let the provider
  reissue the mutation. The guarded composition observes one effect, zero
  sibling calls, zero provider re-entry, and terminal `ErrOutcomeUnknown`.
- The combined regression composes protected effectful tools in this required
  order: `tool.ApprovalRequiredFunc(maf_hitl.ClassifyEffectful(inner))`. Approval
  remains required; after approval an ambiguous response yields one outward
  write and blocks the sibling and model retry.

## Glue versus framework

The framework supplies the agent loop, session serialization, history,
approval snapshot/binding, tool autocall, and MCP transport. The spike adapter
supplies one PostgreSQL pending-interaction table and claim transaction, typed
principal/kind admission, a schema-only clarification tool, and a conservative
effectful-tool error wrapper. `toolautocall` must use
`MaximumConsecutiveErrorsPerRequest: 0` and serial invocation for guarded
effectful batches.

The non-test adapter is 201 lines (`store.go` 174 and `outcome_guard.go` 27).
Process orchestration, MCP fixtures, and assertions live in the test files and
are excluded from that count; this is not a production code-size estimate.

The clarification record is application-owned; Go MAF has no first-class
question/options content type. The schema-tool/tool-result seam is public and
executable, but the product must own presentation and answer validation.

## Limits

- The transaction consumes a reply before restored MAF execution and does not
  persist the answer/decision or a continuation-run record. A crash after claim
  and before dispatch can lose/strand the response. It proves planned wait
  across process death and one response claim, not exactly-once effects under
  arbitrary crashes.
- Arbitrary crash-mid-tool continuation is not covered. No cancellation claim is
  made for already-started concurrent siblings; guarded effectful batches are
  explicitly serial.
- MCP `isError` application payload policy was not classified here. The guard
  covers Go transport errors after an effect may have occurred.
- The spike exercises approval conversion/binding inside `toolautocall`; it does
  not import or prove the separate `agent/harness/toolapproval` middleware.
- Approval timeout behavior is not exercised.
- AG-UI wiring, Java MCP interoperability, Kubernetes scheduling, definition
  lineage/pinning, registry loading, and production migrations are outside this
  spike. The provider is deterministic and uses no paid model key.

## Provenance and reproduction

- Base: `4569dba84f21797ae57053c26603f54621785b26`
- Branch: `codex/spike-maf-hitl`
- Worktree: `/private/tmp/agent-framework-go-maf-hitl-spike`
- Primary raw log: `/private/tmp/agent-framework-go-maf-hitl-spike-final.log`
- Scratch PostgreSQL provenance used during development:
  `/private/tmp/agent-platform-spike-postgres.json`

From the worktree, this creates and removes a fresh PostgreSQL 16 container,
runs only the spike package, and writes the raw log:

```bash
./spikes/maf_hitl/run.sh
```

To reuse an already isolated database, set `MAF_SPIKE_DATABASE_URL` before the
same command. A missing database URL in direct `go test` is an environment skip,
not a pass.

The final log contains eight behavioral scenarios and two no-op helper
entrypoints used by subprocess tests.

Focused framework regression command used by the spike:

```bash
go test ./agent/harness/toolautocall ./agent/harness/toolapproval ./tool/mcptool
```

The first competing-process test run was red with PostgreSQL SQLSTATE `40001`
under serializable isolation. The green implementation uses read committed plus
`SELECT ... FOR UPDATE`, checks the scanned `consumed_at`, and requires exactly
one affected update row. The interactive red output was not saved to a file;
the primary raw log contains the final complete green run.
