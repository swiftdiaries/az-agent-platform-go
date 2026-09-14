# Hub-and-Spoke Agent Platform Design

Status: **approved design; Q1-Q13 recorded; qualified MAF HITL/retry spike reproduced; implementation plan reduced to six deliverables**
Repository revision: `4569dba84f21797ae57053c26603f54621785b26`
Date: 2026-09-14

## Purpose

Replace the technology-debt LangGraph/chat backend with a small Go service. Microsoft Agent Framework (MAF) is the leading implementation candidate, subject to a 0-to-1 seam test against a smaller product-specific harness. The Java monolith remains the source of business capabilities and exposes them as MCP tools.

The service has two deep modules:

- **Chat** owns the AG-UI input and event-delivery boundary, external conversation identity, and the user-facing projection.
- **Agent** owns hub routing, journey execution, per-journey sessions, model context construction, skills, and MCP tool calls.

They communicate through a narrow in-process interface. A journey is configuration, not a new framework abstraction: a stable name, system prompt reference, external MCP tool allowlist, and skill set. MCP owns each tool's schema. The hub selects a journey subagent and can send it later corrections.

## Confirmed needs

- A Go agent platform replaces the existing LangGraph/chat backend; using MAF is a candidate decision.
- The platform remains general purpose and independent of product, journey, and MCP tool schema. Vacation planning and shift-swap self-service are reference journeys only.
- The Java monolith exposes business operations over MCP.
- Chat is a deep module with AG-UI as its UI input seam.
- Agent is a separate deep module called in-process by Chat.
- Conversation threads persist so a user can return later.
- Journey context persists across turns, with at most one active specialist in a V1 conversation. Persistent context does not mean a run is active.
- V1 supports reconnecting to a still-running turn as well as resuming an idle saved conversation.
- V1 runs as a Kubernetes Deployment and Service with multiple replicas. Any replica accepts UI requests; sticky sessions are not required.
- Java MCP calls run with the current user's permissions from UI-supplied headers validated by the existing product auth integration.
- Traces correlate work across Chat, Agent, model calls, and MCP tools.
- Each declarative journey defines a system prompt, MCP tool set, and skills.
- A hub routes work to journey subagents. The meaning of the current “slots” is not yet specified and is not assumed to mean concurrent or addressable agent instances.
- While a journey is active, a user correction is accepted and delivered with existing context at the next safe point.
- Most journeys can ask structured clarification questions and request explicit action approval. A durable wait may span pod replacement and hours or days, then resume the same pinned journey context on any replica without repeating completed tools.

The final sentence does **not** promise cancellation of a running model call or tool. For text steering, MAF's `MessageInjector` queues against an `agent.Session` for a later provider call (`agent/messageinjection.go:21-23,69-95`). The repository test proves loop ordering when a tool itself enqueues before returning (`agent/harness/toolautocall/autocall_test.go:1720-1755`); it does not prove concurrent HTTP admission racing a blocked tool and run completion. The product implementation must prove that race. Attachment and structured-steering semantics remain unspecified.

## Decisions resolved by the grill

V1 uses multiple replicas with one execution owner for each conversation's active specialist/run. Any replica can accept, steer, authorize, or serve reconnect state through PostgreSQL. The exact claim/lease mechanism is an implementation proposal to validate, not a new product choice.

Addressing and handoff are settled: messages route through the hub and may carry an optional explicit journey target; both use the same internal path. The UI adapter parses user-facing mention syntax into a stable target ID. The hub sends an explicit task/message with selected conversation context, and the journey continues from its own retained history. An individual journey may define structured content inside that message, but the platform does not define journey-specific fields.

Journey definitions are versioned files compiled at startup. Existing journey context remains pinned to its immutable definition version; new journeys use the new version, so referenced old bundles must remain available. The version identifies the prompt, tool allowlist, and skill bundle. It does not freeze behavior behind an external model or MCP server reference. Skills are immutable `SKILL.md` packages: metadata is available for selection, instructions and configured supporting files load on demand, and executable actions are separately registered tools. Runtime skill installation, arbitrary file access, and shell execution are outside this contract.

PostgreSQL stores conversation commands, persistent journey context, run state, and sequenced reconnect events. Object-store snapshots are deferred to future asynchronous long-running workflows. The decided V1 recovery boundary is idle resume and live reconnect; automatic execution recovery after process death is deferred.

The UI supplies the current user's auth headers on each request. The Chat auth adapter validates the existing product auth context and passes a request-scoped credential carrier to Agent. Approved headers live only in the owning run's memory and are forwarded on MCP requests for that run; raw credentials never enter a journey definition, transcript, durable inbox, event, or trace. A steering or reconnect request through another replica authorizes the same thread principal and persists only its nonsecret command. An idle or new run requires fresh ingress auth. Credentials are not shared mutable state on a global MCP client. If credentials expire mid-run and no current authorized carrier is available to the owner, the run emits `auth_required`; the platform does not invent refresh-token storage.

Clarification and approval use one durable human-interaction mechanism. A question answer supplies information only. An approval is a separate one-time decision bound to the exact proposed tool call, arguments, principal, conversation/journey context, and definition version. Changed arguments invalidate it; denial, timeout, and custom text never imply permission.

## Approaches

### A. Stock MAF composition

Use a hub MAF agent and expose each journey agent as a tool. This is the shortest route from a user message to a specialist. MAF's existing `agenttool.New` surface, however, accepts a query and returns a string with static run options (`tool/agenttool/agenttool.go:24-38,84-98`). It does not by itself give every user/thread/journey combination a durable subagent session or an addressable mailbox.

This is attractive only if V1 is single-turn delegation with no continuing journey identity. It does not meet the confirmed steering requirement without an application-owned session registry and wrapper, at which point the apparent simplicity is mostly gone.

### B. Thin application supervisor over MAF agents (proposed, conditional)

Keep the hub and each journey as ordinary MAF agents. Add one application-owned supervisor inside Agent that binds a conversation and routed journey to a dedicated `agent.Session`. MAF's `MessageInjector` proves that provider-boundary injection fits the loop, but it cannot acknowledge exact consumption and can accept just after a run's final drain. A tiny Agent-owned inbox middleware must drain durable commands immediately before a provider request, record which communication IDs entered that request, and wake a follow-up turn when a command arrives at the closing-run boundary.

This is the proposed starting point. The first bounded spike qualified MAF's session, human-wait, approval, and uncertain-write seams with application glue; streaming, skills, observability, AG-UI, Kubernetes, and Java integration still need the product slice. MAF workflow checkpointing remains available for a future journey that is truly a known graph with external-input pauses (`workflow/checkpoint/store.go:20-54`; `workflow/checkpoint/manager.go:74-167`); it is not the default conversation store or proof of tool recovery.

### C. Minimal product harness inspired by Pi and Codex

Build only the product loop, model adapter, tool loop, event stream, and session materialization around existing Go model/MCP libraries. Pi's current durable harness is a reference for separating command admission from drive, serialized mutation, open-operation identity, correction consumption, and snapshot-plus-event attachment; Codex is a reference for identity, command/event ports, queue/wake policy, and correlation. This gives exact control and avoids adapting framework abstractions that do not fit.

This becomes preferable if the spike shows that MAF's stock AG-UI host, agent-tool boundary, or session persistence need replacement anyway. Its cost is owning the model/tool loop, event compatibility, tests, and provider behavior. It must stay product-specific; recreating a general agent framework would repeat the debt being removed.

## Reference synthesis: adopt, adapt, defer

| Reference | Adopt | Adapt for this product | Defer |
|---|---|---|---|
| Go MAF | Provider stream/tool loop, MCP adapter, skills provider, middleware seam, and direct OpenTelemetry support | Persistent product journal, per-conversation journey sessions, exact MCP allowlists, AG-UI hosting policy, and inbox boundary with observed inclusion | Workflow checkpointing until a journey is truly a graph; stock agent-as-tool for continuing journeys unless the spike proves session binding |
| Codex | Durable per-worker conversation identity, typed inbox plus explicit wake, command/event port, and privacy-safe correlation | Fixed hub-to-journey lineage and simple journey configuration | Arbitrary agent tree/path routing, capacity policy, residency machinery, and execution-policy intersection |
| Pi | Admission separate from drive, one serialized mutation line, durable correction IDs with separately observed consumption, open-operation identity, active tool selection, and snapshot plus ordered events | Its durable harness invariants and event vocabulary to a smaller Go database model, AG-UI, Java MCP, and MAF OpenTelemetry | Multi-lane/tree/fork/plugin/Chord/TUI machinery and session-wide watching |
| Kagent | Portable journey declaration separated from runtime, compile-then-run validation, immutable compiled definition digest, and credentials outside declarations | Named local prompts and skills plus explicit Java MCP tool names; reverse Kagent's empty-list default so an empty allowlist fails closed | CRDs, controllers, Harness/Substrate revisions, recursive agent topology, remote artifact supply chain, and plugin machinery |
| Substrate | No V1 runtime mechanism; retain only the future checkpoint lesson that immutable payloads precede a manifest commit marker and a database pointer advances conditionally | Apply that publication invariant only if asynchronous crash-resumable workflows enter scope | Object-store snapshots for conversation history, model context, live event replay, or V1 process-death recovery |

The selection rule is practical: choose approach B only if its spike is smaller than approach C while meeting the same acceptance cases. Use MAF as an execution kernel, not as the owner of product persistence or lifecycle truth.

## Proposed ownership and seams

The module ownership is settled. The concrete PostgreSQL claim/lease mechanism remains a proposal to validate against the acceptance cases.

```text
Browser --AG-UI--> any Chat replica --in-process--> local Agent
                         |                              |
                         +-------- PostgreSQL ----------+
                                                        |
                                              one fenced run owner
                                                        |
                                                        v
                                             Java monolith over MCP
```

### Chat module

Chat owns:

- AG-UI request validation, authentication/authorization handoff, and mapping external IDs to internal opaque IDs;
- the user-facing projection of accepted user messages, visible agent messages, and Agent-owned execution states;
- delivery cursors and a rebuildable event replay projection for reconnecting to a still-running turn;
- translating Agent events into AG-UI events;
- validating every submit, steer, and reconnect request against the thread principal and carrying approved request-scoped auth headers into a newly owned run.

The current MAF AG-UI host passes request messages into a new session and runs the agent with the HTTP request context (`provider/aguiprovider/hosting.go:37-76`). It is a useful protocol adapter, but it is not a persistent thread or disconnect-survivable run supervisor. Because live reconnect is required, Chat must own the hosting/re-attachment policy while Agent keeps the run alive independently of an individual HTTP connection.

### Agent module

Agent owns:

- loading and validating journey declarations;
- hub routing and journey-instance lifecycle, without assigning semantics to the product's current slots;
- a separate MAF session per conversation and ongoing journey instance, if journeys are persistent;
- the authoritative append-only command/event journal: accepted inputs, inbox state, run state, visible outputs, and tool outcomes;
- materializing model context from that journal and provider history;
- serialized inbox delivery and wake-up for steering messages;
- durable pending human interactions, one-time answer consumption, and continuation checkpoints;
- resolving skills and filtering MCP tools before they reach a journey;
- tool-call policy and result normalization;
- model, agent, and tool spans;
- claiming one execution owner for a conversation's active run across replicas and fencing authoritative PostgreSQL writes by ownership token.

The MCP adapter lists all server tools and returns them to the caller; it has no built-in journey allowlist (`tool/mcptool/mcp.go:49-76`). The Agent module must filter the list before attaching tools. MAF skills already separate discovery from loading (`agent/skills/provider.go:41-96,296-359`); journey configuration should select skill names without exposing provider mechanics to Chat.

Chat never writes a second authoritative copy of a command. Agent returns `accepted` only after the command and its unique communication ID commit to the journal. If the process stops between receipt and execution, the same committed command remains pending. Chat renders snapshots from authoritative Agent records. Persist a separate read projection only if measured query cost warrants it.

One invariant closes the accept/run race: admission of a command and transition of its conversation's active run to terminal state are serialized in PostgreSQL. While the owning replica remains alive, a run cannot become terminal while leaving a committed pending command without a scheduled follow-up turn. Every replica may append to the durable inbox; the owner reads it, with database notification allowed only as a latency hint. After owner loss, reopening surfaces the interrupted run and pending commands; it does not automatically continue execution. The product integration test must prove the live race with a separately admitted correction on a non-owner replica while the owner's tool is blocked; MAF's enqueue success does not prove it.

The proposed V1 coordinator uses an owner row `{conversation_id, run_id, owner_pod_uid, owner_epoch, lease_until, definition_version, state}`. Atomic claim increments the epoch. The owner checks epoch and lease before each provider/MCP dispatch, after a tool returns, and on every run/event commit. A lease cannot stop or fence an MCP request already sent to Java, prove its side-effect outcome, or prove the old pod is dead. Lease loss marks the run interrupted and blocks automatic takeover of that same run. Failover continuation is deferred.

A later user-started run may proceed, but it must not blindly retry or reissue an unresolved effectful call from the interrupted run. It surfaces the prior uncertainty until Java provides reconciliation or an explicitly reviewed retry/idempotency path. V1 claims one authoritative database owner, not exactly one physical external action.

### Narrow in-process seam

The seam should express product actions rather than MAF types. Illustrative Go shape:

```go
type Platform interface {
    Submit(context.Context, Command) (Receipt, error)
    Observe(context.Context, Subscription) (EventStream, error)
}
```

`Command` carries an opaque thread ID, authenticated principal identity, unique communication ID, text, optional stable target journey ID, and whether idle work should be woken. A separate request-scoped credential carrier may accompany a command to a local owner but is never serialized with it. Chat translates UI mention syntax into the target ID; Agent does not parse presentation syntax. Targeted and inferred routing enter the same hub path. `Receipt` reports **accepted/pending** after the authoritative commit. A later event may report **included in provider request** only when the boundary hook observed that fact; successful enqueue alone is insufficient. Neither state means that a correction was applied retroactively. `Subscription` re-attaches after a sequence cursor and first returns the current run snapshot plus retained later events.

MAF sessions, provider messages, MCP client sessions, and tracing SDK types remain private to Agent.

## State is three different things

| State | Purpose | Proposed authority | Recovery claim |
|---|---|---|---|
| Durable conversation | User messages, visible answers, journey identity, accepted steering, run status, and recorded tool outcomes | Shared PostgreSQL Agent journal; Chat renders a projection | Supports returning to an idle thread from any replica |
| Streaming history | Current run snapshot plus ordered domain execution events after a cursor | Sequenced PostgreSQL Agent events; Chat deterministically encodes AG-UI and tracks delivery cursors | Supports required live reconnect through any replica; the run outlives an individual HTTP request |
| Model context | Selected transcript, journey prompt, skill material, tool calls/results, summaries | Rebuilt by Agent for each provider call | Derived data; never the sole conversation record |
| Human wait | Pending question/approval, pinned provider history and call identity, answer/decision | PostgreSQL Agent journal | Releases the owner; an authorized reply resumes on any replica without replaying completed tools |

A MAF `agent.Session` contains provider-facing history and state, but persistence is delegated to application/provider implementations (`agent/session.go:35-65`; `agent/history.go:34-99`). The platform should persist stable product records and reconstruct a session rather than serialize internal framework objects as its primary schema.

PostgreSQL commits each command and its accepted event once by communication ID. Every visible domain event receives a per-conversation monotonic sequence in the same transaction as its state transition. Reconnect reads a snapshot at watermark `H`, then queries events with `seq > H`; notifications only wake the reader, and the database query closes gaps. Clients deduplicate by sequence, so V1 does not claim exactly-once network delivery.

## Steering sequence

Assume the confirmed one active specialist for this sequence.

1. Chat submits user command `m1`. Agent atomically appends it to the journal, returns `accepted/pending`, and starts run `r1` independently of the HTTP request so a browser can reconnect.
2. The hub selects `vacation-planning-example`. Agent creates or resumes that conversation's pinned journey session and records its lineage.
3. The journey model calls an allowed Java MCP tool whose schema remains external.
4. While that tool is running, the user sends correction `c1`. Agent durably appends `c1` to the same journal/inbox and returns `accepted/pending`.
5. The current tool is allowed to finish. Its result is appended to the journey session. No claim is made that `c1` could cancel or undo the tool.
6. Immediately before the next provider request, Agent's serialized inbox boundary adds `c1`. That request contains the prior messages, the tool call/result, and the correction. Agent records `included in provider request`. If the prior run closed during acceptance, the supervisor wakes a follow-up turn so the correction is not stranded.
7. Agent atomically commits the final visible output and terminal run state, then emits the event. Chat delivers the sequenced event and renders snapshots from Agent records.

This proposes a stronger durable admission contract than Codex's in-memory mailbox. From Codex it retains the queue-versus-wake distinction, a command/event port that isolates UI transport, communication/thread correlation, typed tool outcomes, and privacy-safe telemetry naming. It deliberately omits Codex's arbitrary agent tree and execution-policy machinery, which this product has not asked for.

## Human input and approval

Agent persists a first-class `pending_input` record with a stable interaction ID, one to three short questions, two or three labeled options with descriptions per question, client-added custom text, principal/thread/journey lineage, definition version, and state. Chat maps it to the AG-UI presentation and maps the authorized reply back to the same platform command path. This adopts Codex's question shape and call/turn correlation (`codex-rs/core/src/tools/handlers/request_user_input_spec.rs:9-88,105-127`; `codex-rs/protocol/src/request_user_input.rs:8-70`), while PostgreSQL strengthens its in-memory waiter into durable one-time consumption (`codex-rs/core/src/state/turn.rs:87-113`). AG-UI carries events; it does not own persistence.

An action approval also stores an immutable digest of the exact proposed tool name and arguments. Agent checks the unconsumed approval immediately before dispatch. Any argument change creates a new interaction. Business tools never interpret a clarification answer as approval, and no model text can manufacture an approval record.

Entering `awaiting_input` is a deliberate safe checkpoint before an effectful dispatch. Agent persists the provider history and pending call IDs needed to continue, commits the wait, releases the execution owner, and ends the live run. A later authorized answer atomically consumes the interaction once; another replica claims a new continuation run, restores the pinned state, and continues without replaying completed tools. The reply supplies fresh UI auth headers for the current responding principal; credentials are never serialized in the checkpoint. No lease remains active while waiting.

This guarantee is narrower than arbitrary crash recovery. A planned human wait has a committed continuation boundary. A pod that dies during a model or MCP call still produces the interrupted/unknown semantics above. The seam spike proved session/provider-history restoration, native `toolautocall` approval-call binding, and typed clarification through a non-invocable schema tool plus a matched tool result. It did not exercise `toolapproval.New`. Production must atomically persist the authorized response or decision together with continuation-run admission; the spike's `Claim` consumes the row before that continuation exists, so a crash in between can strand the reply. PostgreSQL durability, timeout policy, and AG-UI mapping remain application work.

### Qualified MAF seam-spike evidence

The isolated worktree `/private/tmp/agent-framework-go-maf-hitl-spike` on `codex/spike-maf-hitl`, based on `4569dba84f21797ae57053c26603f54621785b26`, ran eight behavioral scenarios plus two subprocess helper entrypoints against a fresh PostgreSQL 16 instance, separate OS processes, real MAF sessions and autocall, and a local streamable-HTTP MCP server. The final run and independent reproduction passed; evidence is in `spikes/maf_hitl/README.md`, `/private/tmp/agent-framework-go-maf-hitl-spike-final.log`, and `/private/tmp/agent-framework-go-maf-hitl-spike-independent.log`.

The spike restored the immutable approved action and prior provider history after process replacement; rejected the wrong principal or interaction kind, custom text as approval, forged changed actions, and duplicate consumption; resumed a typed clarification without a magic-text convention; and directly observed fresh request credentials without persisting either credential. Native uncertain-write behavior provided the necessary counterexample: after an applied mutation lost its response, autocall dispatched one sibling and the provider reissued the mutation, producing two mutations. A small application classifier returning `ErrOutcomeUnknown`, with serial invocation and `MaximumConsecutiveErrorsPerRequest: 0`, produced one mutation, no sibling, and no provider re-entry without changing MAF core.

The 201 non-test adapter lines cover only the PostgreSQL interaction store and outcome guard; test helpers contain process coordination, fixtures, and assertions, so this is not a production glue estimate. MCP `isError` with a nil Go error, approval timeout, atomic reply-plus-continuation admission, definition lineage, and actual AG-UI, Kubernetes, Java, and model integration remain unproven. The result qualifies approach B at these two seams; it does not prove arbitrary mid-tool crash recovery or complete the overall framework selection.

## Tool side effects and recovery boundary

Every MCP call gets a stable opaque call ID. The baseline safety rule is conservative: after a live transport failure where a write may have reached Java, the platform reports `outcome_unknown` and does not retry automatically or report success. A retry is safe only when the operation is reviewed as read-only or Java honors the same idempotency key. MCP annotations can inform policy, but the journey's reviewed tool policy is authoritative.

Conversation persistence does not recover a process that died mid-tool. Reopening reports the run interrupted and the external effect `outcome_unknown`; it does not infer success or automatically continue. V1 kills an owner in a test to prove interruption, fencing, and no takeover/replay. A resumable multi-state workflow and automatic reconciliation remain deferred.

## Proposed retry and error policy

The side-effect boundary above is settled. The retry counts, backoff schedule, error codes, and per-turn budgets below are proposed settings for design review.

| Current reference behavior | Lesson for this platform |
|---|---|
| Go MAF executes a tool call once, turns ordinary tool failure into a model-visible function result, and stops after a consecutive-error limit (`agent/harness/toolautocall/autocall.go:1035-1077,1084-1108,1189-1229`). MCP transport failure and an MCP `isError` result are distinct (`tool/mcptool/mcp.go:79-103,483-497`). | MAF supplies useful boundaries but no side-effect-safe tool retry contract. An ambiguous write must stop autocall so the model cannot issue it again. |
| Codex retries model HTTP/stream transport with a configured budget (`codex-rs/model-provider-info/src/lib.rs:30-38,390-447`; `codex-rs/core/src/responses_retry.rs:49-143`). It calls MCP once and distinguishes request failure from returned tool error; failure does not prove the server never received it (`codex-rs/codex-mcp/src/binding.rs:301-365`; `codex-rs/core/src/mcp_tool_call/telemetry.rs:10-18,101-149`). | Reuse model-transport retry separation and MCP uncertainty, not Codex's retry numbers. |
| Pi separately retries provider generation, records tool intent before execution, defaults tool replay to `never`, and resumes only when stored and current replay policy both allow it (`packages/ai/src/utils/provider-retry.ts:22-68,75-123`; `packages/agent/src/harness/runtime/drive/tools.ts:187-228,475-539`). Tool exceptions become one error result without runtime retry (`packages/agent/src/harness/execution/tools.ts:124-159,197-208`). | Reuse the distinction between generation retry and tool replay. Pi does not prove a fresh model call cannot reissue an ambiguous mutation, so this platform explicitly blocks continuation. |

Agent runtime is the single retry owner. Provider/SDK retry settings must be disabled or included in the same total budget so retries do not multiply across SDK, platform, MCP transport, and model behavior. A logical operation ID remains stable across attempts; each attempt has its own attempt ID. An idempotency key helps only when the Java operation actually honors that key. An approval may cover policy-permitted safe retry attempts for the same unchanged logical operation; changed arguments require a new approval.

- A transient model/provider failure may retry with bounded exponential backoff and jitter inside one total deadline.
- A transient MCP failure may retry only when the integration supplies a trustworthy read-only classification or confirms Java idempotency for the stable operation ID. The MCP adapter, not the model, owns those attempts.
- A safe business rejection may return a small structured result to the model so it can repair arguments, within a per-turn tool-error budget. Changed arguments are a new proposed action and require a new approval when approval applies.
- An ambiguous effectful call becomes `outcome_unknown`. Agent withholds it from normal model autocall, ends the run safely, and requires reconciliation or explicit user/product policy before another related action.
- Auth failure becomes `auth_required`. Invalid configuration and unexpected internal failure terminate the run. Chat maps typed recoverable/terminal outcomes to AG-UI and never exposes raw transport errors, stack traces, credentials, or unreviewed Java details. Restricted traces retain an incident/correlation ID.

## Declarative journey shape

This is versioned application configuration, not claimed MAF API:

```yaml
journeys:
  - id: vacation-planning-example
    description: Example journey selected from its user-facing purpose
    system_prompt: prompts/vacation-planning.md
    mcp:
      server: java-monolith
      tools:
        - <exact-tool-name-from-server>
    skills:
      - <journey-skill-name>
```

The vacation label is illustrative; the platform schema contains no vacation, shift, employee, balance, date, leave-approval, or domain-specific operation-class fields. Those semantics stay in the prompt, skill documents, and MCP tool schemas. Startup compiles each definition into an immutable `CompiledJourney` with normalized references and a definition digest. Validation rejects duplicate journey IDs, missing prompts or skills, unknown MCP servers, and tool names not present in the server catalog. Empty or failed allowlists fail closed. Existing context stores the digest and remains pinned; old referenced bundles remain available while referenced.

Each skill is an immutable configured bundle rooted at a `SKILL.md`. Selection reads metadata first; instruction and supporting-file content loads on demand within that bundle only. Executable behavior is an explicitly registered tool, not instructions granted shell or arbitrary filesystem access. Whether MAF's native skill provider satisfies this exact package and pinning contract remains part of the product slice.

Per-user credentials and other runtime state are excluded from definitions and compiled bundles.

Definition bundles are immutable after startup; no in-process activation API or hot reload is needed. Definition rollout is additive. A pod becomes Ready only when it has every database-referenced pinned definition plus the candidate current version. The database marks the new version current only after the new replica set is ready; old bundles remain deployed while referenced. On graceful termination, a pod fails readiness, stops new owner claims, and continues its owned run within the grace period without handoff. A forced or over-grace stop leads to lease expiry and an interrupted run, not automatic continuation.

## Tracing and privacy defaults

Use one trace for a user turn/run, with spans for `chat.accept`, `agent.start_turn`, `hub.route`, `journey.run`, each provider call, and each MCP call. Carry the Go `context.Context` across the in-process seam and through the MCP client call. Record opaque thread, run, journey-instance, communication, and tool-call IDs; journey ID; model/tool name; status; latency; retry count; and token counts when available.

Do not record prompts, user text, skill bodies, MCP arguments/results, authorization material, or raw business identifiers by default. Content capture requires an explicit environment policy with redaction and retention. MAF's OpenTelemetry provider already makes sensitive-data emission opt-in and documents that it includes prompts and completions (`provider/otelprovider/otel.go:93-126`). The application should keep that option off in production.

Propagating a trace through the MCP transport and Java server must be verified against the selected MCP transport. If transport-level trace context is unavailable, correlate the Go client span with an opaque call ID that Java logs and returns. Do not claim a single distributed trace until an integration test sees both sides.

## V1 and deferred scope

### Proposed V1

- Vacation planning and shift-swap self-service as configuration/test references, without platform fields or APIs for either domain.
- Persistent journey context with one active specialist and at most one active run per conversation. The hub may infer routing, but no nested specialist execution runs concurrently in V1.
- Multiple Kubernetes replicas with any-replica ingress, PostgreSQL owner epoch/lease coordination, and no sticky-session dependency.
- PostgreSQL command, context, run-state, and monotonic domain-event persistence.
- Versioned startup-compiled journey definitions pinned by retained context, plus metadata-first on-demand `SKILL.md` packages.
- Current-user MCP credentials held only in the owning run's memory and built into each request without mutating a shared client.
- Durable clarification and exact-action approval waits that release ownership and resume once on any replica.
- Declarative prompt, exact MCP allowlist, and skill selection.
- Durable idle-thread continuation across service restart.
- Snapshot-plus-sequenced-event reattachment to a still-running turn after browser disconnect.
- Durable correction admission plus safe-point delivery to an active journey, with the accept/complete race proven by the product integration tests.
- Explicit `accepted/pending` receipt and an `included in provider request` event only when directly observed.
- If the first journey uses an effectful tool: stable call ID and no blind retry after an uncertain transport outcome.
- End-to-end trace correlation through the Go process and call-ID correlation with Java.
- AG-UI event streaming for the connected client.

### Deferred unless the grill promotes them

- Several simultaneously active specialists or nested specialist execution; all semantics of the existing slots.
- Automatic continuation after process death mid-model or mid-tool.
- General workflow graphs, arbitrary subagent trees, dynamic agent creation, and cross-thread messaging.
- Runtime journey editing/hot reload, config version migration, long-term memory, semantic retrieval, and automatic transcript compaction.
- Generic plugin marketplace or user-authored journeys.

## 0-to-1 slice and acceptance

The first slice should configure a vacation-planning reference journey against test MCP schemas, then configure shift-swap self-service without changing a platform API or storage schema. Add an effectful path only when an external MCP operation needs one.

1. A test AG-UI client starts a PostgreSQL-backed thread. The hub infers or accepts an explicit journey target; only that version's declared tools and skills are visible, and MCP requests carry the current user's allowed headers.
2. Configure vacation planning, then shift-swap self-service, without changing a platform API or storage schema. Restart after a completed run and continue the same pinned journey context without the client resending its transcript.
3. With a tool blocked on replica A, submit steering through replica B and reconnect through replica C. Release the tool so admission races final drain. The next provider request includes the tool result and correction; reconnect from snapshot watermark `H` yields every later sequence in order, with duplicates removed by sequence.
4. Let replica A lose PostgreSQL access after dispatching to Java while Java remains reachable. Replica C never takes over or replays that run; A's fenced database result commit is rejected, and the UI shows `interrupted` plus `outcome_unknown` without claiming the external call was canceled.
5. Roll out an additive definition version. New pods become Ready with current and pinned bundles before the database activates it; old context remains loadable. Graceful drain stops new claims and lets owned work finish within grace. Killing A beyond grace makes its run interrupted without handoff or automatic continuation.
6. Expire a run's request-scoped credentials. The owner emits `auth_required`; no raw header appears in PostgreSQL, definitions, events, or traces, and a shared MCP client is never mutated with per-user state.
7. Assert one correlated trace across Chat, hub, journey, provider, and MCP client spans, plus the same opaque tool-call ID in the Java test server. Assert that prompt and tool payload content is absent under production defaults.
8. Ask a structured question, release the owner, replace the pod, answer through another replica, and prove the pinned journey continues once without repeating prior tools. Repeat with approval: changed arguments require a new approval; deny, timeout, custom text, and duplicate answers never dispatch the protected tool.
9. Make a classified safe read fail transiently before succeeding. Assert bounded backoff, one stable logical operation ID, distinct attempt IDs, one total retry budget, and no hidden SDK/MCP retry multiplication.
10. Let an effectful call fail ambiguously after dispatch. Assert no automatic attempt, no model-reissued call, terminal `outcome_unknown`, and no success claim. A later action requires the explicit reconciliation/product path.
11. Return a safe structured business rejection, let the model repair its arguments once within budget, and assert a changed approved action asks again. Transport/auth/internal details never become model or UI content.
12. Return expired auth and an unexpected internal error in separate runs. Assert `auth_required` versus sanitized terminal failure, no retry, and correlation to restricted traces.

Unit tests cover journey compilation and pinning, fail-closed allowlist filtering, conversation/session isolation, communication-ID deduplication, monotonic event sequences, inbox admission versus run completion, and epoch-fenced state transitions. Integration tests use a controllable model stub, controllable MCP server, PostgreSQL, the real AG-UI HTTP boundary, and at least three service replicas. Process-death execution continuation remains explicitly outside V1.

## Evidence and uncertainties

- Go MAF source evidence is pinned to `4569dba84f21797ae57053c26603f54621785b26` in this repository. The Go feature comparison reports no handoff builder (`README.md:52`; `docs/dotnet-go-sdk-feature-comparison.md:95`), supporting a small application supervisor rather than assuming a missing primitive.
- Microsoft Agent Framework documentation was consulted through Context7 for sessions, AG-UI, MCP/skills, workflows, and observability; it returned Python/.NET detail for several concepts, so the checked Go source and [Go package reference](https://pkg.go.dev/github.com/microsoft/agent-framework-go) are authoritative for Go capability.
- AG-UI human-input documentation was consulted through Context7. The protocol supplies frontend tool/event transport, not the platform's PostgreSQL wait state, authorization, replica ownership, or continuation guarantee; the current Go host remains tied to an HTTP request (`provider/aguiprovider/hosting.go:37-76`).
- Codex comparison is pinned to local checkout `/Users/adhita/projects/python/src/github.com/swiftdiaries/codex` at `5b1d6560181680f95cde95c14ed042acc02248ed`. The queue/wake split is defined in `codex-rs/core/src/tools/handlers/multi_agents_spec.rs:181-237`, and model-boundary draining in `codex-rs/core/src/session/turn.rs:414-434,494-565`. It is a behavioral reference, not a dependency or promise of feature parity.
- Pi comparison is pinned to verified local and remote `earendil-works/pi` main at `ceea48f5d5d12fd7915dfefba2835ccd55f23bb9` (2026-09-14). Durable construction and open-operation recovery are in `packages/agent/src/harness/runtime/harness.ts:374-407`; serialized mutation is in `packages/agent/src/harness/session/types.ts:508-547`; durable correction admission/consumption is in `packages/agent/src/harness/runtime/lane.ts:1418-1545`; journey snapshot/events are in `packages/agent/src/harness/runtime/lane.ts:1705-1725`. Session-wide `watchSession` alone remains stubbed (`packages/agent/src/harness/runtime/harness.ts:305-307`).
- Kagent comparison is pinned to verified local/remote main `2d843e3732c308d7728c4c7daa1708a96277f159`. Its portable declaration/runtime split is in `go/api/v1alpha3/agenttemplate_types.go:188-215` and `go/api/v1alpha3/harness_types.go:119-196`; its MCP binding treats an empty tool list as the full server (`go/api/v1alpha3/agenttemplate_types.go:56-74`), which this design deliberately reverses. Kagent informs the compile boundary only; its CRD/runtime topology is not adopted.
- Substrate comparison is pinned to local snapshot `672533541dbfcd29084e4de2475267088bda3651`. Payload-before-manifest publication is in `cmd/atelet/main.go:790-827`, conditional database pointer update in `cmd/ateapi/internal/controlapi/workflow_suspend.go:396-445`, and sandbox snapshot scope in `cmd/atelet/sandbox_assets.go:73-105`. It informs only future asynchronous-workflow checkpoints; it contributes no V1 thread storage or proof of a Java MCP effect.
- PostgreSQL and the existing product auth integration are selected. Exact Java MCP transport/header mapping and model provider remain integration inputs rather than platform-domain choices.
- The MAF HITL/retry spike is pinned to worktree branch `codex/spike-maf-hitl` at base `4569dba84f21797ae57053c26603f54621785b26`. Its report and preserved author/independent logs establish the qualified seam result above; the main framework source was unchanged, while the spike worktree added its artifacts and `pgx` dependency only.
- The supplied [ATOSS leave-planning page](https://www.atoss.com/en/insights/blog/leave-planning) describes leave management through employee self-service. It is scenario context only and contributes no platform fields, workflow, or product requirements.

## Review status

All thirteen product questions in the grill are recorded, and the user approved this design on 2026-09-14. The qualified spike removes the earlier uncertainty about whether current MAF can express the planned human-wait and uncertain-write boundaries with small application adapters. Complete MAF selection, retry settings/taxonomy, atomic reply-plus-continuation admission, and the concrete PostgreSQL lease implementation remain subject to the product slice and acceptance tests above. The user requested a YAGNI trim on 2026-09-14. The [six-deliverable plan](../plans/2026-09-14-agent-platform/index.md) replaces the incomplete 40-task draft while retaining every acceptance case. The current work revises planning documents and does not begin platform implementation.
