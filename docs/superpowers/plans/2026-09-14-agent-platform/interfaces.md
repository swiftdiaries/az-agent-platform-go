# Chat–Agent boundary

Chat owns authenticated AG-UI ingress, external ID mapping and event delivery. Agent owns commands, sessions, execution and lifecycle truth. Render the Chat snapshot from authoritative Agent records; add persisted projections only after measured query cost justifies them.

## Stable seam

Keep `Submit` and `Observe` under `internal/platform`. Define their concrete command, receipt, snapshot and event types when their first callers are implemented; this is an in-process seam, not a public Go SDK.

- `Submit` accepts a validated command and a separate memory-only credential carrier. Commands contain conversation/principal/communication identity, kind, text or interaction reply, optional journey target and wake policy. A receipt means committed acceptance, never provider consumption.
- `Observe` accepts authenticated conversation identity and a sequence cursor. It returns a consistent snapshot/watermark and a closeable, context-cancelable stream of later events. Authorize every observation.
- MAF sessions, provider messages, MCP sessions and telemetry SDK types remain inside Agent. Do not predeclare Compiler, Registry, Router, ContextBuilder or Supervisor interfaces; use concrete implementations and introduce consumer-local interfaces only where needed.

## Required state semantics

- `pending -> running -> completed` is normal. Claim an explicit pending run ID, never an expired running run. At most one pending/running execution exists per conversation.
- `running -> awaiting_input` atomically saves continuation history and pending call identity, creates the interaction/event and releases ownership. Enforce open-wait uniqueness separately from live execution.
- Owner loss marks the run `interrupted` without automatic takeover or replay. Auth/internal failures and ambiguous effects also stop execution. Record operation outcome separately: an interrupted run may also contain an `outcome_unknown` effect. A later user-authorized run must preserve unresolved effect uncertainty.
- Claim, finish, wait and reply transactions enforce their own permitted transitions. No generic public `Transition` API or separate `CloseRun` wrapper is needed.
- Admission and finish lock the same conversation. If finish wins, admission schedules the follow-up; if admission wins, finish sees it. While the owner lives, committed pending input cannot be stranded. After owner loss, pending work is surfaced without automatic continuation.
- Every authoritative owned write checks epoch and lease using PostgreSQL's current clock after lock acquisition. Check before provider/MCP dispatch as well; database fencing cannot undo an external effect.
- A human reply validates principal, kind, expiry and payload, consumes the interaction, records the reply/event and admits one continuation in a single transaction. Identical retries return the original receipt/continuation; changed payload under the same communication ID is rejected.
- Clarification contains one to three questions, each with two or three labeled/described options and optional custom text. Approval contains an immutable server-stored call, not clarification questions. Bind it to exact arguments, tool, principal, journey/conversation and definition digest; reject replacement calls from clients.
- Deterministic argument binding must preserve JSON numbers and reject trailing values. Denial, timeout, custom text and changed arguments never authorize dispatch.
- Persist stable product records and provider-history JSON, not raw credentials. Add schema/types only as each deliverable needs them; enforce uniqueness, foreign keys and state checks in PostgreSQL.

## Definition and execution boundaries

Compile strict journey configuration at startup into immutable digest-indexed bundles. Keep all referenced bundles deployed; existing sessions resolve their pinned digest and only new sessions use the database's current version. Readiness checks retained/candidate availability before deployment activation. No in-process bundle mutation or hot reload.

Startup validates symbolic tool selections against checked-in server contracts without user credentials. Each run performs authenticated discovery and exact fail-closed binding with fresh credentials. MCP owns tool schemas. Use strict configuration decoding and semantic checks; no duplicate journey JSON Schema is needed without a consumer.

Retain the full retry/error taxonomy and privacy defaults from the spec. Use one retry budget, stable logical operation IDs and distinct attempt IDs. An uncertain effect must terminate outside normal model autocall. Provider-request inclusion is recorded only after direct boundary observation, never merely after context construction.
