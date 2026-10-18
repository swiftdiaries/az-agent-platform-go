package runtime

import (
	"context"
	"slices"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// traced installs a recording global tracer provider (restored on cleanup) and makes
// the harness drive runs inside a "journey.run" span. The returned function ends that
// span and returns the recorded spans.
func (h *harness) traced() func() []sdktrace.ReadOnlySpan {
	h.t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	h.t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
	})
	ctx, span := otel.Tracer("test").Start(context.Background(), "journey.run")
	h.ctx = ctx
	return func() []sdktrace.ReadOnlySpan {
		span.End()
		return recorder.Ended()
	}
}

func spanNamed(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("no span %q", name)
	return nil
}

type recordedEvent map[string]attribute.Value

func eventsOf(spans []sdktrace.ReadOnlySpan, name string) (out []recordedEvent) {
	for _, span := range spans {
		for _, e := range span.Events() {
			if e.Name != name {
				continue
			}
			attrs := recordedEvent{}
			for _, a := range e.Attributes {
				attrs[string(a.Key)] = a.Value
			}
			out = append(out, attrs)
		}
	}
	return out
}

func TestTelemetryActivationEvent(t *testing.T) {
	h := newHarness(t)
	end := h.traced()
	seedActive(t, h, "alpha", "beta")
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "activate_journey", `{"journey":"gamma"}`)),
		answer("done"),
	}
	if _, err := h.run("", 2); err != nil {
		t.Fatal(err)
	}
	spans := end()
	events := eventsOf(spans, "journey.activated")
	if len(events) != 1 {
		t.Fatalf("journey.activated events = %v", events)
	}
	e := events[0]
	if e["journey.id"].AsString() != "gamma" || e["journeys.limit"].AsInt64() != 2 || e["journeys.trigger"].AsString() != "tool" || !slices.Equal(e["journeys.evicted"].AsStringSlice(), []string{"alpha"}) {
		t.Fatalf("event = %v", e)
	}
	hasEvent := func(name string) bool {
		return slices.ContainsFunc(spanNamed(t, spans, "journey.run").Events(), func(e sdktrace.Event) bool { return e.Name == name })
	}
	if !hasEvent("journey.activated") {
		t.Fatal("journey.activated is not on the journey.run span")
	}
	if len(eventsOf(spans, "journey.evicted")) != 0 {
		t.Fatal("unexpected fit eviction event")
	}
}

func TestTelemetryTargetActivationEventAndLimitAttribute(t *testing.T) {
	h := newHarness(t)
	end := h.traced()
	h.model.script = []func(ModelRequest) ModelResponse{answer("done")}
	if _, err := h.run("beta", 3); err != nil {
		t.Fatal(err)
	}
	spans := end()
	events := eventsOf(spans, "journey.activated")
	if len(events) != 1 || events[0]["journeys.trigger"].AsString() != "target" || events[0]["journey.id"].AsString() != "beta" {
		t.Fatalf("events = %v", events)
	}
	if _, ok := events[0]["journeys.evicted"]; ok {
		t.Fatal("empty eviction list must be omitted")
	}
	found := false
	for _, a := range spanNamed(t, spans, "journey.run").Attributes() {
		if a.Key == "journeys.limit" && a.Value.AsInt64() == 3 {
			found = true
		}
	}
	if !found {
		t.Fatalf("journeys.limit attribute missing: %v", spans[0].Attributes())
	}
}

func TestTelemetryActivationRefusedEvent(t *testing.T) {
	h := newHarness(t)
	end := h.traced()
	h.model.script = []func(ModelRequest) ModelResponse{respond(toolCall("c1", "alpha_book", `{}`))}
	if out, err := h.run("alpha", 1); err != nil || !out.Waiting {
		t.Fatalf("setup: %+v %v", out, err)
	}
	h.approvePending()
	conv := &conversation{set: h.set, cat: h.cat, limit: 1, available: h.set.Journeys()}
	if _, err := conv.activate(h.runCtx(), RunInput{Store: h.store, Owner: h.owner}, `{"journey":"beta"}`); err != nil {
		t.Fatal(err)
	}
	events := eventsOf(end(), "journey.activation_refused")
	if len(events) != 1 || events[0]["journey.id"].AsString() != "beta" || events[0]["journeys.limit"].AsInt64() != 1 {
		t.Fatalf("events = %v", events)
	}
}

func TestTelemetryFitEvictionEvent(t *testing.T) {
	h := newHarness(t)
	end := h.traced()
	seedActive(t, h, "alpha", "beta", "gamma")
	h.model.script = []func(ModelRequest) ModelResponse{answer("done")}
	if _, err := h.run("", 2); err != nil {
		t.Fatal(err)
	}
	events := eventsOf(end(), "journey.evicted")
	if len(events) != 1 {
		t.Fatalf("events = %v", events)
	}
	e := events[0]
	if !slices.Equal(e["journeys.evicted"].AsStringSlice(), []string{"alpha"}) || e["journeys.limit"].AsInt64() != 2 || e["journeys.trigger"].AsString() != "fit" {
		t.Fatalf("event = %v", e)
	}
}

func TestTelemetryHistoryTruncatedEvent(t *testing.T) {
	h := newHarness(t)
	end := h.traced()
	h.model.script = []func(ModelRequest) ModelResponse{answer("done")}
	history := seededHistory(4)
	if _, err := h.runSeeded(history, estimateTokens(history[4:])+50); err != nil {
		t.Fatal(err)
	}
	events := eventsOf(end(), "history.truncated")
	if len(events) != 1 {
		t.Fatalf("events = %v", events)
	}
	e := events[0]
	if e["turns_dropped"].AsInt64() != 2 || e["messages_dropped"].AsInt64() != 4 || e["estimate_after"].AsInt64() >= e["estimate_before"].AsInt64() || e["budget"].AsInt64() == 0 {
		t.Fatalf("event = %v", e)
	}
	if len(e) != 5 {
		t.Fatalf("event carries more than counts: %v", e)
	}
}
