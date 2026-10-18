# az-agent-platform-go

A small Go service that runs one **conversation agent** per conversation thread. The Java monolith stays the source of business capabilities and exposes them as MCP tools; this service owns the AG-UI boundary, the conversation, journey activation, tool catalog resolution, settings, cost attribution and MCP calls. The name does not require Azure hosting.

- Design (the end state): [docs/design.md](docs/design.md). Domain language: [CONTEXT.md](CONTEXT.md).
- How the code got there, the stage table and the risks: [docs/migration.md](docs/migration.md).
- What is proven and what is blocked: [docs/acceptance.md](docs/acceptance.md).
- Deploy (Compose, Kubernetes): [deploy/README.md](deploy/README.md). Real-boundary evidence: [docs/e2e-setup.md](docs/e2e-setup.md).
- The earlier hub-and-spoke design under `docs/superpowers/` is historical; [Provenance](docs/PROVENANCE.md) records where it came from.

## The conversation agent

Every message goes to the conversation's one agent, which streams directly to the user. There is no router and no handoff. See [Conversation agent and journeys](docs/design.md#conversation-agent-and-journeys).

- **Prompts.** `conversation_prompt` in the definitions YAML names the base prompt file (`configs/conversation.md`). Each journey's `SKILL.md` body is its instructions; the model sees only each journey's name and description until it activates one.
- **`activate_journey`.** The model activates a journey by calling it (an optional explicit target on a message activates one before the run). The next round is rebuilt with that journey's tools and instructions. Other calls in the same response are answered "not executed". A call to a tool that is not in the active set returns `{"error":"tool_not_active"}` and is never dispatched. A journey whose operations do not resolve for the user is hidden from the model.
- **Active-set limit.** At most `max_active_journeys` journeys are active (default 2, allowed 1 to 4). It is a per-tenant setting from `AGENT_PLATFORM_SETTINGS`; a user-layer value is clamped to the tenant's value, never raised. Lowering a limit evicts down at the next run start.
- **True LRU.** Activating a journey, dispatching one of its tools and reading its resources with `read_skill_resource` all make it most recently used. A call answered `tool_not_active` touches nothing. Activation beyond the limit evicts the least recently used unpinned journey.
- **Pinning.** A journey with a pending or decided-but-undispatched approval, or an unresolved `outcome_unknown` or `dispatching` operation, is pinned and never evicted. If every active journey is pinned, activation is refused (the model sees `{"error":"activation_refused"}`).

## Journey definitions

Strict YAML, compiled at startup into a digest-addressed set ([Journey definition](docs/design.md#journey-definition)); `configs/journeys.yaml` is a working example.

```yaml
conversation_prompt: conversation.md
catalog:
  operation_meta_key: operation_key      # _meta key holding the operation key (integration input)
  call_id_header: X-Platform-Call-ID     # header carrying the opaque call ID to Java
journeys:
  - id: vacation-planning
    skill: skills/vacation-planning/     # directory with SKILL.md
    product_versions: {min: "2026.2", max_exclusive: "2027.1"}
    max_tools: 15                        # default 15, at most 64
    deny: [leave.cancel]                 # operation keys this journey never exposes
    tools:
      - op: leave.balance
        class: read_only                 # read_only | idempotent | effectful
      - op: [leave.request.v2, leave.request]   # alternatives, preferred first
        alias: leave_request             # required: more than one alternative
        approval: required               # the overlay can only narrow
      - tags: [leave]                    # tag selector: no class, no alias
        read_only: true                  # hint filter, not a class
```

- **`alias:`** is the stable model-facing name of a selector. It is required on an op selector with more than one alternative; a single-key selector defaults to a name derived from the key (non-alphanumerics become `_`). It stays the same when the tenant resolves a different alternative or a newer key is prepended. See [Aliases](docs/design.md#aliases).
- **`class:` is allowed only on `op:` selectors.** A tool matched only by tag selectors is `effectful` (approval required, never retried); a tag match adds only `approval: required`. `alias:` is also op-only. An unclassified operation is `effectful`. See [Policy classes](docs/design.md#policy-classes-and-approval).
- **`approval: required`** adds approval to any operation. `deny` removes operation keys. `max_tools` bounds a journey's resolved tools. `product_versions` is a half-open range; a journey out of range for the tenant is unavailable.
- A selector that resolves to nothing, or to more than one tool, makes the journey unavailable to that user instead of failing mid-turn.

## Approvals and clarification

See [Human waits and approvals](docs/design.md#human-waits-and-approvals).

- **Markers:** `requested` (at the wait), `decided` (at the reply, no network I/O), `dispatched` (at the guard, exactly once), `denied`, `superseded` (the catalog, product version, schema, name or definition set changed).
- **Journey-bound.** An approval is consumed only under the journey it was proposed in. If that journey is no longer available the model sees `{"error":"approval_superseded"}` and nothing is dispatched.
- **`policy_conflict`.** One operation key or alias carrying different effective policies in two journeys is a compile error for op selectors; for tag matches it is detected at bind time and the affected journey is unavailable.
- **Continuation target.** A reply continuation whose target journey is no longer available does not fail the run; the target is simply not activated and the approval guard decides.
- **Run scope first.** Every `Wait` requires the run's scope to be recorded (`RecordRunScope`: product version, endpoint, settings) beforehand.
- A clarification supplies information only; it never authorizes an action and it survives a product-version flip.

## Errors and outcomes

See [Effects, retry and errors](docs/design.md#effects-retry-and-errors).

| Outcome | When | Effect |
| --- | --- | --- |
| `outcome_unknown` | An effectful call may have reached Java and its result is lost | No retry, the run ends, the journey is pinned, and the thread-level stop is kept: a later run on the thread is refused while an operation is unresolved |
| `auth_required` | 401 on any MCP request, or 403 on `initialize`/`tools/list` (at `Catalog.Open`) | The run ends; the Binding is marked broken; no refresh token is stored |
| `not_permitted` | 403 on `tools/call` | A model-visible result `{"error":"not_permitted"}`; one MCP call, no retry, the run continues and other tools stay callable. The test server's `Forbid` control (`internal/mcptest`) produces it |
| `dependency_unavailable` | Directory, allowlist, endpoint or settings failure at run start | The run ends before any dispatch; no credential is sent |
| `limit_exceeded` | See [Cost](#cost) | The run ends; never retried in-run |

## Cost

See [Cost seam](docs/design.md#cost-seam). The platform owns attribution, typed outcomes and usage records; an external enforcement point owns budgets and pricing.

- `AGENT_PLATFORM_COST_GATE`: `open` (default, admits everything, adds nothing) or `headers`.
- `AGENT_PLATFORM_COST_HEADERS`: `key=Header-Name` pairs with keys `tenant`, `principal`, `conversation`, `run`, `journey`, `profile`. Blank uses `X-Agent-Tenant`, `X-Agent-Principal`, `X-Agent-Conversation`, `X-Agent-Run`, `X-Agent-Journey`, `X-Agent-Cost-Profile`.
- **Trust.** The enforcement point must accept attribution headers **only from Agent** (mTLS or network policy). Otherwise the identifiers are spoofable.
- **`limit_exceeded`.** The AG-UI stream gets a CUSTOM event `limit_exceeded` with `{kind, retryAfterSeconds}`, then RUN_ERROR with code `limit_exceeded`. `kind` is `budget` or `rate`. A 429 is a limit (budget when the body carries `budget_exceeded`, otherwise rate). LiteLLM reports an exhausted budget as HTTP 400 `budget_exceeded`, which is a limit; any other 400 is not. An `Admit` denial ends the run the same way before any model call.
- **Usage.** One `agent_usage` row is written per successful model call (tokens and attribution); `cost.Usage` is the read side. The platform computes no currency.
- **Order.** `Admit` runs after Binding, so attribution carries only a validated journey. A run without an explicit target attributes an empty journey.

## History budget

`AGENT_PLATFORM_HISTORY_BUDGET` is `model=tokens` pairs, required, and must include the configured model (`AZ_AGENT_FOUNDRY_DEPLOYMENT` or `AZ_AGENT_LITELLM_MODEL`). Tokens are estimated at about 4 bytes per token. Before each provider request the oldest whole turns beyond the budget are left out of that request, never splitting a call from its result; the latest turn and turns an open wait or unresolved operation references are kept. One `history_truncated` event carries counts only. The persisted history is never trimmed. See [Context budget](docs/design.md#context-budget).

## Definitions rollout

See [Rollout](docs/design.md#journey-definition) and [Definitions](docs/design.md#definitions).

- A conversation pins a definition set by digest. An **idle** conversation (no pending interaction, not a reply continuation, no unresolved operation) re-pins to the current set at its next run start and a `definition_changed` event records the old and new digest. A conversation that is not idle keeps its pin, because approval bindings include the set digest.
- **Bounded readiness.** A pod is Ready when it serves the current set plus every set referenced by an open wait (a pending interaction, or a consumed one whose continuation has not finished) or by an unresolved operation. Idle conversations do not count. This replaces the older "retain every pinned set" rule.
- Keep older configs loadable with `AGENT_PLATFORM_RETAINED_CONFIGS` (comma-separated paths to the same YAML schema) for as long as such conversations exist. Any journey or config change must keep the previous config retained until then; otherwise the pod is not Ready or a continuation fails closed. A retained config with an old schema (for example a multi-alternative op selector without `alias:`, or `class:` on a tag selector) no longer compiles.
- Definitions are loaded once at startup; changing a mounted file needs a restart.

## Configuration

Secrets (`DATABASE_URL`, model credentials) come from Secret references, never config files, journey YAML, logs or traces. Startup errors are fixed messages that never echo values. Names come from `cmd/agent-platform/main.go`; a full example is `deploy/config/agent-platform.env.example`.

| Variable | Required | Default | Purpose |
| --- | --- | --- | --- |
| `DATABASE_URL` | yes | none | PostgreSQL connection string (migrations run at startup) |
| `AGENT_PLATFORM_PORT` | yes | none | Listener port (1 to 65535) |
| `AGENT_PLATFORM_SHUTDOWN_GRACE` | yes | none | Graceful shutdown duration, for example `25s` |
| `AGENT_PLATFORM_CONFIG` | yes | none | Current definitions YAML |
| `AGENT_PLATFORM_RETAINED_CONFIGS` | no | none | Comma-separated older definitions YAML paths |
| `AGENT_PLATFORM_HISTORY_BUDGET` | yes | none | `model=tokens` pairs; must include the configured model |
| `AGENT_PLATFORM_TENANT_DIRECTORY` | yes | none | Tenant directory YAML: tenant to `product_version` and `endpoint` ID |
| `AGENT_PLATFORM_MCP_ENDPOINTS` | yes | none | Endpoint allowlist `id=url[,id2=url2]`; URLs live only here |
| `AGENT_PLATFORM_SETTINGS` | yes | none | Settings YAML: `default`, `tenants`, `users` layers |
| `AGENT_PLATFORM_CREDENTIAL_HEADERS` | no | none (nothing forwarded) | Inbound header names copied into the credential sent to MCP |
| `AGENT_PLATFORM_COST_GATE` | no | `open` | `open` or `headers` |
| `AGENT_PLATFORM_COST_HEADERS` | no | `X-Agent-*` names | Attribution header names for `headers` |
| `AZ_AGENT_MODEL_PROVIDER` | no | `foundry` | `foundry` or `litellm` |
| `AZ_AGENT_FOUNDRY_PROJECT_ENDPOINT`, `AZ_AGENT_FOUNDRY_DEPLOYMENT` | with `foundry` | none | Foundry project endpoint and deployment (Azure credentials come from the environment or workload identity) |
| `AZ_AGENT_LITELLM_BASE_URL`, `AZ_AGENT_LITELLM_API_KEY`, `AZ_AGENT_LITELLM_MODEL` | with `litellm` | none | LiteLLM proxy URL, key and model alias |
| `KEYCLOAK_JWT_ISSUER`, `KEYCLOAK_JWKS_URL`, `KEYCLOAK_JWT_AUDIENCE`, `KEYCLOAK_JWT_SOURCE`, `KEYCLOAK_JWT_HEADER`, `KEYCLOAK_USER_CLAIM_PATH`, `KEYCLOAK_TENANT_CLAIM_PATH`, `KEYCLOAK_JWT_ALLOWED_ALGORITHMS` | yes | none | Ingress JWT validation and identity claims |
| `OTEL_SERVICE_NAME`, `OTEL_SERVICE_VERSION` | yes | none | Telemetry resource |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS` | no | none (no export) | Full OTLP HTTP traces URL (including `/v1/traces`) and `k=v` headers |

Hard-coded integration inputs, with no environment knob: the catalog snapshot TTL is 10 minutes (`catalog.DefaultSnapshotTTL`), and tag selectors read the `_meta["tags"]` key. The operation-key `_meta` name is configured (`catalog.operation_meta_key`). These and the other open inputs are listed in [Integration inputs](docs/design.md#integration-inputs).

### Model provider through LiteLLM

Point the app at a LiteLLM proxy whose `model_name` alias fronts an Azure OpenAI deployment:

```yaml
model_list:
  - model_name: azure-alias
    litellm_params:
      model: azure/<deployment-name>
      api_base: os.environ/AZURE_API_BASE
      api_key: os.environ/AZURE_API_KEY
      api_version: os.environ/AZURE_API_VERSION
```

```dotenv
AZ_AGENT_MODEL_PROVIDER=litellm
AZ_AGENT_LITELLM_BASE_URL=http://localhost:4000
AZ_AGENT_LITELLM_API_KEY=proxy-api-key
AZ_AGENT_LITELLM_MODEL=azure-alias
AGENT_PLATFORM_HISTORY_BUDGET=azure-alias=64000
```

The app sends `store: false` and its history on each Responses request. See [LiteLLM's Azure Responses guide](https://docs.litellm.ai/docs/providers/azure/azure_responses).

## Telemetry

Spans: `chat.accept`, `agent.start_turn`, `journey.run` (carries `journey.id` only for targeted runs), `catalog.resolve`, `model.call` and `mcp.call` (attributes `tool.alias`, `operation.key`, `call.id` and `outcome`). Span events on `journey.run`: `journey.activated`, `journey.activation_refused`, `journey.evicted` and `history.truncated`. The `telemetry.Redact` span processor drops `gen_ai.tool.description` before export. Nothing records prompts, user text, arguments or results, credentials, server tool descriptions or remote tool names. A single distributed trace into Java is not claimed yet (see [acceptance.md](docs/acceptance.md#telemetry)). See [Tracing and privacy](docs/design.md#tracing-and-privacy).

## Recorded decisions

- An `Admit` denial can still re-pin an idle conversation to the current definition set and emit `definition_changed`; this is accepted.
- A mid-run re-pin across an `ErrPending` iteration is accepted.
- LiteLLM HTTP 400 `budget_exceeded` is a limit, not a generic provider error.

## Local TUI

After the local Compose stack is running ([deploy/README.md](deploy/README.md)):

```sh
export KEYCLOAK_ACCESS_TOKEN=...
go run ./cmd/agent-platform-tui --journey vacation-planner
```

`--url` selects the API (or `AGENT_PLATFORM_URL`, default `http://localhost:8080`). `--journey` selects a journey. `--thread THREAD` starts a new run on an existing thread; add `--run RUN` to resume that run's event stream (`--run` requires `--thread`). Enter sends a message or chooses a clarification option; approval prompts accept `approve` or `deny`; PgUp/PgDown scroll; Ctrl+C quits. If a stream ends before a terminal event, `/retry` resends the exact request.

## Testing

Tests need PostgreSQL 16. `internal/pgtest` starts a `postgres:16` container, or uses an isolated database named by `TEST_DATABASE_URL` (the role must be able to `CREATE DATABASE`; never point it at a shared or production database). A missing database reports BLOCKED, not a pass.

```sh
export TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable   # optional
go test -race -count=1 ./...
go vet ./...
```

The black-box check is `TestE2EConversationAgent` (`integration/e2e_test.go`, stages S0 to S9). The progress ratchet is `integration/e2e_progress_test.go`: its `e2eReachedStage` constant says which stages must pass; later stages skip as pending. The per-case record is [docs/acceptance.md](docs/acceptance.md). CI (`.github/workflows/test.yaml`) runs build, vet, `gofmt` and `go test -race -count=1 ./...` against a `postgres:16` service on every push and pull request. Real Keycloak, Foundry, LiteLLM, Java MCP, a cost gateway and Kubernetes remain BLOCKED.
