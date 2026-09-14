package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	platformmcp "github.com/swiftdiaries/az-agent-platform-go/internal/mcp"
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
	var mu sync.Mutex
	var observations []http.Header
	server := mcp.NewServer(&mcp.Implementation{Name: "java-fixture", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "lookup_destination"}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
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
	bound, err := platformmcp.NewClient().Bind(ctx, definitions.MCPServer{
		ID:           "java-fixture",
		Endpoint:     backend.URL,
		Tools:        []string{"lookup_destination"},
		CallIDHeader: "X-Platform-Call-ID",
	}, []string{"lookup_destination"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	if _, err := bound.Call(ctx, callID, "lookup_destination", json.RawMessage(`{"destination":"secret business-id"}`)); err != nil {
		t.Fatal(err)
	}
	root.End()

	mu.Lock()
	gotHeaders := slices.Clone(observations)
	mu.Unlock()
	if len(gotHeaders) != 1 {
		t.Fatalf("tools/call observations = %d, want 1", len(gotHeaders))
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
	if got := gotHeaders[0].Get("X-Platform-Call-ID"); got != callID {
		t.Fatalf("Java call ID = %q, want %q", got, callID)
	}
	for _, span := range recorder.Ended() {
		for _, attribute := range span.Attributes() {
			if strings.Contains(attribute.Value.AsString(), "secret") || strings.Contains(attribute.Value.AsString(), "business-id") {
				t.Fatalf("sensitive content leaked into span %q attribute %q", span.Name(), attribute.Key)
			}
		}
	}
}
