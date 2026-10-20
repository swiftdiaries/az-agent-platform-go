package telemetry

import (
	"context"
	"slices"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// RedactedAttributes lists the attribute keys Redact drops: gen_ai.tool.description is
// a server-supplied description and is never exported.
var RedactedAttributes = []attribute.Key{"gen_ai.tool.description"}

// Redact wraps next so that every ended span reaches it without the attributes in
// RedactedAttributes (span attributes and span-event attributes).
func Redact(next sdktrace.SpanProcessor) sdktrace.SpanProcessor {
	return redactor{next: next}
}

type redactor struct{ next sdktrace.SpanProcessor }

func (r redactor) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) { r.next.OnStart(ctx, s) }
func (r redactor) OnEnd(s sdktrace.ReadOnlySpan)                         { r.next.OnEnd(redactedSpan{s}) }
func (r redactor) Shutdown(ctx context.Context) error                    { return r.next.Shutdown(ctx) }
func (r redactor) ForceFlush(ctx context.Context) error                  { return r.next.ForceFlush(ctx) }

type redactedSpan struct{ sdktrace.ReadOnlySpan }

func (s redactedSpan) Attributes() []attribute.KeyValue {
	return withoutRedacted(s.ReadOnlySpan.Attributes())
}

func (s redactedSpan) Events() []sdktrace.Event {
	events := s.ReadOnlySpan.Events()
	out := make([]sdktrace.Event, len(events))
	for i, e := range events {
		e.Attributes = withoutRedacted(e.Attributes)
		out[i] = e
	}
	return out
}

func withoutRedacted(attrs []attribute.KeyValue) []attribute.KeyValue {
	if !slices.ContainsFunc(attrs, func(a attribute.KeyValue) bool { return slices.Contains(RedactedAttributes, a.Key) }) {
		return attrs
	}
	return slices.DeleteFunc(slices.Clone(attrs), func(a attribute.KeyValue) bool { return slices.Contains(RedactedAttributes, a.Key) })
}
