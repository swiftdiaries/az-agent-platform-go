# Framework baseline for `az-agent-platform-go`

This is a portable, planning-only reference for the standalone module
`github.com/swiftdiaries/az-agent-platform-go`. It records what was verified
against the checked-out Go framework, rather than treating the .NET or Python
implementations as the Go API.

## Provenance and reproducible framework dependency

| Item | Verified value |
| --- | --- |
| Framework source module | `github.com/microsoft/agent-framework-go` |
| Checked-out source revision | `4569dba84f21797ae57053c26603f54621785b26` |
| Commit timestamp | `2026-09-14T05:05:36Z` |
| Source remote | `github.com/swiftdiaries/agent-framework-go` |
| Remote verification | `git ls-remote` reports that revision as the fork's `HEAD` and `refs/heads/main` |
| Declared / resolved toolchain | Go `1.26.0` / `go1.26.0` |
| MAF fork pseudo-version | `v0.0.0-20260914050536-4569dba84f21` |

The source is a fork but declares the Microsoft module path. The standalone
module must preserve that identity and use this explicit, versioned replacement:

```go
require github.com/microsoft/agent-framework-go v0.0.0-20260914050536-4569dba84f21

replace github.com/microsoft/agent-framework-go => github.com/swiftdiaries/agent-framework-go v0.0.0-20260914050536-4569dba84f21
```

This was checked in an otherwise empty temporary module, with isolated Go module
and build caches: `go mod download -json` and `go list -m -json` both resolved
the replacement. The downloaded fork module has zip checksum
`h1:v7prD4QC/UfBjSg/aQ5Tqb5HOnP41ByWUBMmm2k+rIE=` and `go.mod` checksum
`h1:uBBWLtWLv1+1a4kwUINQWIrpYCpe1CSJohDJnTBZClU=`. Bootstrap should commit
the resulting `go.sum`; it must not use a machine-local or `/private/tmp`
replacement.

The framework's current direct-dependency baseline is:

```text
github.com/modelcontextprotocol/go-sdk v1.7.0
github.com/ag-ui-protocol/ag-ui/sdks/community/go v0.0.0-20260312103001-8e7ab1df34c8
go.opentelemetry.io/otel v1.46.0
go.opentelemetry.io/otel/sdk v1.46.0
```

These versions are source-baseline evidence, not a directive to add every one
as a direct platform dependency. Add the MCP SDK directly to construct MCP
transports, the AG-UI SDK when consuming its types directly, and the OTel SDK
when configuring an exporter/provider. No PostgreSQL, router, authentication,
or provider-SDK version is selected by this reference.

## Verified Go seams

| Need | Current Go API and behavior | Planning consequence |
| --- | --- | --- |
| Create and restore a conversation | `a.CreateSession(ctx, options...)`; `json.Marshal(session)` and `json.Unmarshal(data, &session)` are supported. `a.RunText(ctx, text, agent.WithSession(session)).Collect()` runs it. | The platform owns the session row, revision/control transaction, and when the serialized bytes are committed. A session is agent-specific. |
| Tool auto-call and approval | `tool.ApprovalRequiredFunc(fn)` marks a `tool.FuncTool`; `toolautocall.Config` has `AllowConcurrentInvocations`, `MaximumConsecutiveErrorsPerRequest`, and default-on approval-response binding. | For protected/effectful execution use serial calls (`AllowConcurrentInvocations: false`) and `MaximumConsecutiveErrorsPerRequest: &zero`. Do not set `DisableApprovalResponseBinding`. The product must decide its own durable approval/principal protocol. |
| Native approval reply | A pause is represented by `*message.ToolApprovalRequestContent`; reply with `request.CreateResponse(approved, reason)`. The autocall state keys are persisted in `agent.Session`. | Carry the framework request/response only inside the restored continuation. Persist an application interaction record with principal, kind, and an exact action snapshot before returning a human-wait response. |
| Typed clarification | A known `tool.SchemaTool` that is not invocable stops the auto-call loop. No first-class question/options content type exists. | Model clarification as an application-owned schema-only `request_clarification` tool plus durable question/options/answer validation. It is not a framework approval. |
| AG-UI server | `aguiprovider.NewJSONHTTPHandler(hostedAgent, aguiprovider.HandlerConfig{})` is an `http.Handler`. It accepts a POSTed AG-UI `RunAgentInput` and emits SSE. | The handler converts input messages and calls `hostedAgent.Run` without `agent.WithSession`; it is transport-only. Do not use AG-UI `threadID` as durable MAF-session ownership. The platform HTTP layer must load/save the session and authenticate the caller. |
| MCP tool discovery/call | `mcptool.Connect(ctx, transport)` returns `*mcp.ClientSession`; `mcptool.ListTools(ctx, session)` exposes remote tools. Its wrapper calls `session.CallTool(ctx, &mcp.CallToolParams{Name: remoteName, Arguments: json.RawMessage(args)})`. | Apply policy/approval/outcome wrappers around the returned `tool.FuncTool` values before adding them to an agent. Preserve the remote name in audit data: the framework normalizes provider-visible tool names and rejects normalization collisions. |
| MCP result taxonomy | A non-nil error from `CallTool` is the transport/dispatch failure path. A successful protocol reply can still be an application-level MCP error (`CallToolResult.IsError`) and is converted into agent content. | The platform's effectful-tool guard must classify uncertain transport outcomes conservatively. Do not conflate a received MCP error result with an unknown external effect. |
| MCP authorization | The official Go SDK documentation for `v1.7.0` shows `mcp.StreamableClientTransport{Endpoint: ..., OAuthHandler: handler}`. The OAuth handler supplies `Authorization: Bearer` and drives authorization on 401/403. | Start only a fresh credentialed MCP transport after the durable interaction is admitted for resumed execution. Header/static-token injection requires a concrete v1.7.0 transport test before it is selected; this reference verifies OAuth only. |
| Tracing | `otelprovider.NewMiddleware(otelprovider.MiddlewareConfig{SourceName: ...})` is an `agent.Middleware`; it obtains global `otel.Tracer` and `otel.Meter`, emits GenAI spans/metrics, and forwards a tracer into tool calls. | Install an application tracer provider/exporter before constructing agents. Add platform correlation identifiers in platform middleware/spans; do not assume MAF persists trace context across durable waits. |
| Skills | `skills.NewContextProvider(skills.ContextProviderOptions{Sources: ...})` registers skill tools. `fsskills.NewSource(...)` is a source implementation. `load_skill`, `read_skill_resource`, and `run_skill_script` are approval-required by default. | Treat skills and scripts as effectful capability surfaces. Keep their source roots and allow-lists platform-owned; do not disable their approval flags without a reviewed policy. |

Relevant local source locations at the recorded revision are:

- `agent/agent.go:239` (session creation) and `agent/session.go:9` (JSON contract)
- `agent/harness/toolautocall/autocall.go:46` (configuration and safety defaults)
- `tool/tool.go:89` (approval marker) and `message` tool-approval content
- `provider/aguiprovider/hosting.go:25` (AG-UI HTTP/SSE host)
- `tool/mcptool/mcp.go:40` and `tool/mcptool/mcp.go:483` (MCP connect/list/call)
- `provider/otelprovider/otel.go:24` (OTel middleware)
- `agent/skills/provider.go:63` and `agent/skills/provider.go:426` (skill approval defaults)

Context7 was also queried for current official MCP Go SDK and AG-UI documentation.
It confirms `StreamableClientTransport`/`OAuthHandler` behavior and documents the
community Go AG-UI SDK. The local Go implementation above remains authoritative
for MAF APIs; Context7's Microsoft framework material was .NET/Python-oriented.

## Production integration constraints still pending

1. A database transaction that atomically persists session bytes, interaction
   state, and continuation intent is platform work. Framework JSON support alone
   does not supply concurrency control or recovery semantics.
2. The AG-UI host does not authenticate users, bind principals, restore MAF
   sessions, or save their state. Those responsibilities need explicit HTTP and
   persistence adapters.
3. The exact mechanism for static bearer/API-key headers in MCP v1.7.0 has not
   been chosen or verified here. OAuth is the verified first option.
4. Durable answer claim then process death before framework dispatch remains a
   recovery design problem. The copied spike demonstrates one-claim admission,
   not exactly-once effect execution across arbitrary crashes.
5. Do not put raw credentials in the serialized MAF session or the pending
   interaction row. Recreate a credentialed MCP client only after admission.

## Portable HITL spike evidence (nonproduction)

`spike/` is copied from the prior isolated worktree at the same source revision
so the new standalone project does not depend on `/private/tmp`. It is reference
material only: Go source files have a `.go.txt` suffix so root `go test ./...`
cannot compile them as product packages. It contains no raw run logs, cache
contents, database URL, or credentials.

- `spike/README.md` records scope, behavior matrix, known limits, and the
  original reproduction command.
- `spike/store.go.txt` and `spike/outcome_guard.go.txt` are the small adapter
  evidence: PostgreSQL `FOR UPDATE` interaction claim and conservative
  effectful-tool error guard.
- `spike/native_test.go.txt`, `spike/process_integration_test.go.txt`,
  `spike/store_integration_test.go.txt`, and
  `spike/ambiguous_failure_test.go.txt` are executable evidence of session
  restore, authorization binding, competing resumes, and ambiguous effect
  handling. They are not production tests or a production schema.

Original spike file SHA-256 values:

```text
README.md                    7cb8054e22967b05a83bf35554a16dada93a0d13e698d319e440c8481d05b477
store.go                     b4764b6a821a98793b612ffa0576d4dda5e015ff88212155b288494ef66d6c1e
outcome_guard.go             ce1f892cbd3a6bcdeb7784390dfd1aa52cab093baebff4bcce26ef356f77071b
native_test.go                097494c091fe827623b4abe82b0c9690dca95928efee52ec6cbfeef9e06d224e
process_integration_test.go   bb7e7b98e8b3bc03b10f13f06fdf4e252c5fe64b0cb67c90ca2d25f191f31fde
ambiguous_failure_test.go     7858016462de61c7ce573c5bbd86111e906f09dff7cbccbcb646c995e8171f70
store_integration_test.go     d01d28f85085e0a9e72643b771ee4a5724021f3e367e333ca5be295cd5e266e5
```

## Framework evidence hashes

```text
go.mod                                      1740e245dc427f1582c08f18674d466d986aa22aad62e6af84825c96848bd1c6
agent/session.go                            04b20bb4832d5f36ed40f8aadff3c1bd1e3f7a2bdae0bd619e3a0028ec54bdd6
agent/harness/toolautocall/autocall.go      5cad4c1074413860362ae57090d871ee81133fe218922de7edc38f8faf8a1351
agent/harness/toolapproval/toolapproval.go  a6d8614964c56955a22edb85edccc589510f3196d6db76b55f2f7301f4ce1644
tool/mcptool/mcp.go                         0ca194e3f45e36ca1b46f27a0c2f68310a9124fb9562d55e450a76abb741b562
provider/aguiprovider/hosting.go            b40b216684c260d7b02807f61b888ee15a7a655a12015f69479de1ab27fb7fb5
provider/aguiprovider/agui.go               221746020dd95c6ec04b2dee7494f0d39a0ff5c5e6a19bba9c6ad8d93b423c9c
provider/otelprovider/otel.go               4affa76abd15dbfd1f2728a6b58e9bc47fb058c6869c1f75aee1f1bb7c1ac271
agent/skills/provider.go                    bd088ec24f34bdbb4340b98cdb270c41704e91c0614d94be54e3bc8fe60f184f
```
