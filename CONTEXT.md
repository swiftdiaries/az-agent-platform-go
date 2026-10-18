# Agent Platform

Language for the general-purpose agent platform that replaces the existing LangGraph-based agent backend. The design is in [docs/design.md](docs/design.md).

## Language

### Conversation

**Agent platform**:
The domain-agnostic service that runs one conversation agent per conversation thread. Vacation planning and shift swapping are reference journeys, not concepts built into the platform.

**Chat backend**:
The deep module that accepts UI input through AG-UI and exposes a narrow in-process seam to the agent platform.

**Conversation thread**:
A persistent conversation that a user can leave, resume later, or reconnect to while its current turn is still running.

**Conversation agent**:
The single agent that serves a conversation thread, holding its one retained history and activating journeys as the conversation needs them.
_Avoid_: hub, orchestrator, router, specialist, subagent

**Steering**:
A correction or additional instruction sent while the conversation agent is running. It is delivered with the conversation context at the next safe point.

### Journeys

**Journey**:
A declarative bundle for one product use case: instructions loaded on demand, a tool selector, a policy overlay, and supporting files. Tool input and output schemas belong to the MCP server.
_Avoid_: use case, specialist, skill (when meaning the whole bundle)

**Active journeys**:
The journeys whose instructions and tools are currently in play for a conversation. At most N are active at once, where N is the tenant's `max_active_journeys` setting (default 2). The least recently used unpinned journey is evicted first, where a journey is used when it is activated, one of its tools is called, or its supporting files are read; a journey with an open approval or uncertain effect is pinned and stays active.
_Avoid_: active specialist

**Explicit target**:
An optional stable journey identifier supplied with a user message that activates that journey directly. UI mention syntax is outside the agent platform language.

**Definition version**:
An immutable compiled set of journeys (a `definitions.Set`, identified by its digest), pinned by a conversation and re-pinned to the current set at a run start when the conversation is idle (no open wait, no unresolved operation). It identifies instructions, tool selectors, and policy overlays, but not the tools a given user can reach, which come from that user's catalog snapshot.

### Tools and products

**Operation key**:
A stable identifier the MCP server publishes for each tool, naming a business operation's contract independently of the tenant- or user-specific tool name. A breaking change to the contract gets a new operation key.
_Avoid_: capability, tool ID, tool name

**Tool selector**:
A journey's declaration of which operation keys it may use, optionally narrowed by server-published tags or side-effect hints. It never names a tenant-specific tool.
_Avoid_: allowlist (when meaning exact tool names)

**Policy overlay**:
Platform-owned rules for a journey's operations that can only narrow what the server publishes, such as denying an operation, requiring approval, or setting its policy class.

**Policy class**:
The reviewed retry and approval class the overlay gives an operation: `read_only`, `idempotent` or `effectful`. Approval is required unless the class is `read_only`. Only `read_only` and `idempotent` operations are retried. MCP annotations inform but never grant a class.

**Tool catalog**:
The tools the MCP server exposes to one user at one product version. Its tool names are dynamic per tenant and user.
_Avoid_: tool list, server contract

**Catalog snapshot**:
A user's tool catalog captured when a conversation's first message is handled, identified by digest and revalidated at each run start. It never grants permission; the MCP server still authorizes every call.

**Tool alias**:
The stable model-facing name for a tool selector's operation, the same on every tenant and product version whichever of the selector's operation keys resolves. A selector with alternative operation keys declares its alias; a single key defaults to a name derived from it. It is mapped to the user's dynamic tool name at call time.

**Product version**:
The release of the product a tenant runs. Each product version has its own MCP server version and endpoint.
_Avoid_: MCP version (as a separate concept)

**Tenant directory**:
The authoritative mapping from a tenant to its product version and MCP endpoint. The endpoint is an ID that a deploy-time allowlist turns into a URL before any credential is sent.

### Settings and cost

**Settings**:
Tenant and user configuration that a settings source supplies and an API can later supply, layered deploy default, then tenant, then user, where a user value can only narrow the tenant's limits. Settings are typed, carry a version digest, and are resolved once at run start and recorded on the run. The first setting is `max_active_journeys`; the second is the opaque `cost.profile`.

**Attribution**:
The opaque identifiers (tenant, principal, conversation, run, journey) that tag a run's model calls and usage so an enforcement point can count them. It never contains credentials or content.

**Enforcement point**:
The component outside the platform that checks and charges budgets for model calls, such as a gateway with an Envoy or Linkerd rate-limit service, LiteLLM virtual keys, or nothing. It owns budgets, counting and pricing; the platform owns attribution, typed limit outcomes and a usage record.

**Cost gate**:
The platform port through which a run asks whether it may start and obtains the opaque, redacting access decoration for its model calls. Adapters: open (admits everything), headers (attribution headers), and a scripted one for tests. A denial or an enforcement-point rejection ends the run as `limit_exceeded`.

### Human interaction

**Human interaction**:
A durable request for one user's input that pauses the conversation agent at a safe boundary. It is either a clarification question or an explicit action approval.

**Clarification question**:
A structured question with choices and optional custom text. Its answer supplies information; it does not authorize an action.

**Action approval**:
A one-time allow or deny decision bound to one exact proposed operation and its arguments, at one product version. Changed arguments, a changed operation contract, or a product version change require a new approval.

**Awaiting input**:
A durable conversation state with no active execution owner. An authorized answer can later resume the pinned conversation on any replica without replaying completed tools.
