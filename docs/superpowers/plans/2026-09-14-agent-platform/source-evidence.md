# Portable dependency and API evidence

All implementation sessions use this note; none depends on the source repository or `/private/tmp` spike checkout.

## Reproducible module baseline

Use Go 1.26.0 and:

```go
require github.com/microsoft/agent-framework-go v0.0.0-20260914050536-4569dba84f21
replace github.com/microsoft/agent-framework-go => github.com/swiftdiaries/agent-framework-go v0.0.0-20260914050536-4569dba84f21
```

The source commit is `4569dba84f21797ae57053c26603f54621785b26` (2026-09-14T05:05:36Z). A clean Go 1.26 module download and `go list -m` resolved the fork pseudo-version. Recorded sums: module `h1:v7prD4QC/UfBjSg/aQ5Tqb5HOnP41ByWUBMmm2k+rIE=` and go.mod `h1:uBBWLtWLv1+1a4kwUINQWIrpYCpe1CSJohDJnTBZClU=`. Task 1 must verify these live and stop `BLOCKED` on mismatch.

Direct transitive APIs at the pin use MCP Go SDK v1.7.0, AG-UI Go SDK `v0.0.0-20260312103001-8e7ab1df34c8`, and OpenTelemetry Go v1.46.0.

## Exact MAF seams

- Session: `a.CreateSession(ctx, opts...) (*agent.Session,error)`; JSON round trip uses `json.Marshal(session)` and `json.Unmarshal(data,&session)`; run with `a.RunText(ctx,text,agent.WithSession(session)).Collect()` or `RunMessage`.
- Tool loop: `toolautocall.New(toolautocall.Config{AllowConcurrentInvocations:false, MaximumConsecutiveErrorsPerRequest:&zero})`. Approval binding remains enabled; do not set `DisableApprovalResponseBinding`.
- Approval: wrap `tool.FuncTool` with `tool.ApprovalRequiredFunc(fn)`; inspect `*message.ToolApprovalRequestContent`; the authoritative request creates `request.CreateResponse(approved, reason)`.
- AG-UI adapter: `aguiprovider.NewJSONHTTPHandler(a, aguiprovider.HandlerConfig{}) http.Handler`. It maps one HTTP run to SSE and does not provide the platform's durable session, detached execution, snapshot/replay, or ownership semantics.
- MCP: `mcptool.Connect(ctx, transport)` and `mcptool.ListTools(ctx, session)`; the wrapper ultimately calls `session.CallTool(ctx,&mcp.CallToolParams{Name: originalName, Arguments: json.RawMessage(args)})`. A Go error is transport/dispatch failure. `CallToolResult.IsError` is a successfully returned protocol-level application error and must be classified separately.
- Streamable HTTP OAuth: `mcp.StreamableClientTransport{Endpoint: endpoint, OAuthHandler: handler}`. Product header forwarding uses a per-run `http.RoundTripper`; never mutate a global client.
- OTel: `agent.Config{Middlewares: []agent.Middleware{otelprovider.NewMiddleware(otelprovider.MiddlewareConfig{SourceName: "az-agent-platform"})}}`; keep sensitive-data logging disabled.
- Skills: `skills.NewContextProvider(skills.ContextProviderOptions{Sources: []skills.Source{fsskills.NewSource(os.DirFS(root))}})`. Built-in skill tools require approval by default. This platform does not expose script execution; Task 5 provides a restricted source and only load/read tools.

Portable source detail is in [references/framework-baseline.md](references/framework-baseline.md); preserved spike artifacts are in [references/spike/README.md](references/spike/README.md).

## Evidence limits

The qualified spike established session JSON persistence, native approval binding, typed clarification glue, fresh in-memory credential forwarding, and the counterexample where an ambiguous mutation can be issued twice through unconstrained autocall. It did not establish durable AG-UI hosting, exact concurrent correction consumption, Kubernetes ownership, Java trace propagation, or crash-midtool recovery. Tasks 2–6 close the adopted seams with product tests; crash-midtool automatic continuation remains excluded.
