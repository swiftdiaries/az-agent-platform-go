package catalog

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/swiftdiaries/az-agent-platform-go/internal/mcptest"
)

func TestTelemetryMCPCallOutcome(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
	})

	outcomes := func() (out []string) {
		for _, span := range recorder.Ended() {
			if span.Name() != "mcp.call" {
				continue
			}
			got := ""
			for _, a := range span.Attributes() {
				if a.Key == "outcome" {
					got = a.Value.AsString()
				}
			}
			out = append(out, got)
		}
		return out
	}

	for _, test := range []struct {
		name, want string
		setup      func(*mcptest.Server)
	}{
		{"completed", "completed", func(*mcptest.Server) {}},
		{"business rejected", "business_rejected", func(s *mcptest.Server) { s.IsError("travel/lookup", "nope") }},
		{"auth required", "auth_required", func(s *mcptest.Server) { s.Unauthorized("travel/lookup", -1) }},
		{"not permitted", "not_permitted", func(s *mcptest.Server) { s.Forbid("travel/lookup") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := len(outcomes())
			e := newEnv(t, mcptest.Options{})
			test.setup(e.server)
			b := e.open(t, e.claim(t, "r1"), Options{})
			_, _ = b.Call(t.Context(), "call-1", "travel_lookup", []byte(`{}`))
			got := outcomes()
			if len(got) != before+1 || got[before] != test.want {
				t.Fatalf("outcomes = %v, want last %q", got, test.want)
			}
		})
	}
}
