# Task 6 telemetry

The service uses the OpenTelemetry Go SDK 1.46.0. `internal/telemetry.New` installs a process tracer provider and the W3C Trace Context propagator. With no endpoint configured, the provider keeps export disabled. With an OTLP HTTP endpoint configured, it uses the SDK batch exporter and copies configured exporter headers without putting them in span data.

```go
provider, err := telemetry.New(telemetry.Config{
	ServiceName:     "az-agent-platform",
	ServiceVersion:  revision,
	Endpoint:        os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	Headers:         exporterHeadersFromSecret,
	ShutdownTimeout: 5 * time.Second,
})
if err != nil {
	return err
}
defer provider.Shutdown(context.Background())
```

The shutdown bound applies to exporter flush as well as provider shutdown. An earlier caller deadline still wins. Deployment should provide the endpoint and service identity through non-secret configuration, and exporter authentication headers through a Secret reference.

MCP request transport injects only `traceparent` and `tracestate` from the active context. The platform call ID remains the configured opaque `X-Platform-Call-ID` value. Existing `chat.accept`, `agent.start_turn`, `hub.route`, `journey.run`, `model.call`, `mcp.discover`, and `mcp.call` spans retain their ownership and parent chain. Prompts, user text, skill bodies, tool arguments, credentials, and raw business identifiers are not span attributes or propagation values.

`integration/telemetry_test.go` records spans in memory and uses a fake Java MCP HTTP endpoint. It proves that the MCP request carries a valid W3C trace context and the same opaque call ID, while sensitive tool content is absent from recorded spans. It is fixture evidence only; no real Java service or distributed-trace claim is available until the configured boundary is supplied.
