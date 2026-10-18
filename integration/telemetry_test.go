package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/credential"
	"github.com/swiftdiaries/az-agent-platform-go/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestTelemetryInitializerInstallsW3CPropagation(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})

	provider, err := telemetry.New(telemetry.Config{ServiceName: "telemetry-test", ShutdownTimeout: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, span := otel.Tracer("test").Start(t.Context(), "root")
	defer span.End()
	header := make(http.Header)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(header))
	if header.Get("traceparent") == "" {
		t.Fatal("telemetry initializer did not install W3C traceparent propagation")
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetryMCPTraceAndCallIDAreCorrelatedWithoutContent(t *testing.T) {
	// The call-ID header name comes from catalog.call_id_header in the YAML config:
	// the default name, a custom name, and (for a read_only tool) no header at all.
	for _, test := range []struct {
		name, header string // header "" = the config has no call_id_header
	}{
		{"configured header", "X-Platform-Call-ID"},
		{"custom header", "X-Custom-Correlation"},
		{"no header configured", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			testTelemetryCorrelation(t, test.header)
		})
	}
}

func testTelemetryCorrelation(t *testing.T, header string) {
	var mu sync.Mutex
	var observations []http.Header
	var remoteNames []string
	server := mcp.NewServer(&mcp.Implementation{Name: "java-fixture", Version: "1"}, nil)
	mcp.AddTool(server, keyedTool(remoteLookup, lookupAlias), func(_ context.Context, req *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
		mu.Lock()
		remoteNames = append(remoteNames, req.Params.Name)
		mu.Unlock()
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	})
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read", http.StatusInternalServerError)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if strings.Contains(string(body), `"method":"tools/call"`) {
			mu.Lock()
			observations = append(observations, r.Header.Clone())
			mu.Unlock()
		}
		streamable.ServeHTTP(w, r)
	}))
	defer backend.Close()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
		_ = provider.Shutdown(context.Background())
	})

	ctx, root := otel.Tracer("test").Start(t.Context(), "journey.run")
	callID := "call_opaque_01"
	// An idempotent tool requires call_id_header; a read_only one does not.
	class := "idempotent"
	configPath := writeBundleConfig(t, t.TempDir(), "prompt\n", lookupAlias, class)
	if header == "" {
		class = "read_only"
		configPath = writeBundleConfig(t, t.TempDir(), "prompt\n", lookupAlias, class)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	const defaultHeaderLine = "  call_id_header: X-Platform-Call-ID\n"
	replacement := ""
	if header != "" {
		replacement = "  call_id_header: " + header + "\n"
	}
	edited := strings.Replace(string(data), defaultHeaderLine, replacement, 1)
	if header != "X-Platform-Call-ID" && edited == string(data) {
		t.Fatal("call_id_header edit did not apply")
	}
	if err := os.WriteFile(configPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	set := compileSet(t, configPath)
	if got := set.Catalog().CallIDHeader; got != http.CanonicalHeaderKey(header) {
		t.Fatalf("compiled call_id_header = %q, want %q", got, http.CanonicalHeaderKey(header))
	}
	bound, err := openTestCatalog(ctx, t, testEndpoint(t, backend.URL), credential.Credential{}, set)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close() // before backend.Close; idempotent with the helper's cleanup
	if _, err := bound.Call(ctx, callID, lookupAlias, json.RawMessage(`{"destination":"secret business-id"}`)); err != nil {
		t.Fatal(err)
	}
	root.End()

	mu.Lock()
	gotHeaders := slices.Clone(observations)
	gotRemote := slices.Clone(remoteNames)
	mu.Unlock()
	if len(gotHeaders) != 1 {
		t.Fatalf("tools/call observations = %d, want 1", len(gotHeaders))
	}
	if !slices.Equal(gotRemote, []string{remoteLookup}) {
		t.Fatalf("MCP received tools %v, want the remote name %q (the caller used alias %q)", gotRemote, remoteLookup, lookupAlias)
	}
	carrier := propagation.HeaderCarrier(gotHeaders[0])
	remote := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
	remoteSpan := trace.SpanContextFromContext(remote)
	if !remoteSpan.IsValid() {
		t.Fatal("fake Java did not receive valid W3C trace context")
	}
	if remoteSpan.TraceID() != recorder.Ended()[0].SpanContext().TraceID() {
		t.Fatalf("MCP trace ID = %s, root trace ID = %s", remoteSpan.TraceID(), recorder.Ended()[0].SpanContext().TraceID())
	}
	switch header {
	case "":
		for _, name := range []string{"X-Platform-Call-ID", "X-Custom-Correlation"} {
			if got := gotHeaders[0].Get(name); got != "" {
				t.Fatalf("no call_id_header configured but %s = %q was sent", name, got)
			}
		}
	default:
		if got := gotHeaders[0].Get(header); got != callID {
			t.Fatalf("Java %s = %q, want %q", header, got, callID)
		}
		for _, other := range []string{"X-Platform-Call-ID", "X-Custom-Correlation"} {
			if other != header && gotHeaders[0].Get(other) != "" {
				t.Fatalf("unconfigured header %s was sent", other)
			}
		}
	}
	names := map[string]bool{}
	for _, span := range recorder.Ended() {
		names[span.Name()] = true
		for _, attribute := range span.Attributes() {
			value := attribute.Value.Emit()
			// The remote tool name, like arguments and descriptions, never reaches telemetry.
			if strings.Contains(value, "secret") || strings.Contains(value, "business-id") || strings.Contains(value, remoteLookup) {
				t.Fatalf("sensitive content leaked into span %q attribute %q", span.Name(), attribute.Key)
			}
		}
		if span.Name() == "mcp.call" {
			if spanAttribute(span, "call.id") != callID || spanAttribute(span, "tool.alias") != lookupAlias || spanAttribute(span, "operation.key") != lookupAlias {
				t.Fatalf("mcp.call attributes = %s", spanDump(span))
			}
		}
	}
	if !names["catalog.resolve"] || !names["mcp.call"] {
		t.Fatalf("catalog.resolve/mcp.call spans missing: %v", names)
	}
}
