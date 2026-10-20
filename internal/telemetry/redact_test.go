package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const sentinelDescription = "SENTINEL-server-description"

func redactedProvider(next sdktrace.SpanProcessor) *sdktrace.TracerProvider {
	return sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(Redact(next)))
}

func TestRedactDropsToolDescription(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := redactedProvider(recorder)
	_, span := provider.Tracer("t").Start(context.Background(), "tool")
	span.SetAttributes(
		attribute.String("gen_ai.tool.description", sentinelDescription),
		attribute.String("gen_ai.tool.name", "alias"),
		attribute.String("other", "kept"),
	)
	span.AddEvent("e", trace.WithAttributes(
		attribute.String("gen_ai.tool.description", sentinelDescription),
		attribute.String("kept", "yes"),
	))
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("ended = %d", len(ended))
	}
	got := map[attribute.Key]string{}
	for _, a := range ended[0].Attributes() {
		got[a.Key] = a.Value.Emit()
	}
	if _, ok := got["gen_ai.tool.description"]; ok {
		t.Fatal("description reached the processor")
	}
	if got["gen_ai.tool.name"] != "alias" || got["other"] != "kept" {
		t.Fatalf("attributes = %v", got)
	}
	events := ended[0].Events()
	if len(events) != 1 || len(events[0].Attributes) != 1 || events[0].Attributes[0].Key != "kept" {
		t.Fatalf("events = %+v", events)
	}
}

func TestRedactPassesThroughOtherwise(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := redactedProvider(recorder)
	_, span := provider.Tracer("t").Start(context.Background(), "plain")
	span.SetAttributes(attribute.String("a", "1"), attribute.Int("b", 2))
	span.AddEvent("e", trace.WithAttributes(attribute.String("c", "3")))
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 || ended[0].Name() != "plain" || len(ended[0].Attributes()) != 2 {
		t.Fatalf("ended = %+v", ended)
	}
	if events := ended[0].Events(); len(events) != 1 || len(events[0].Attributes) != 1 {
		t.Fatalf("events = %+v", events)
	}
	if len(recorder.Started()) != 1 {
		t.Fatalf("started = %d", len(recorder.Started()))
	}
}

type countingProcessor struct{ shutdowns, flushes int }

func (*countingProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (*countingProcessor) OnEnd(sdktrace.ReadOnlySpan)                     {}
func (c *countingProcessor) Shutdown(context.Context) error                { c.shutdowns++; return nil }
func (c *countingProcessor) ForceFlush(context.Context) error              { c.flushes++; return nil }

func TestRedactDelegatesShutdownAndFlush(t *testing.T) {
	next := &countingProcessor{}
	processor := Redact(next)
	if err := processor.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if next.flushes != 1 || next.shutdowns != 1 {
		t.Fatalf("flushes=%d shutdowns=%d", next.flushes, next.shutdowns)
	}
}
