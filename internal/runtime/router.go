package runtime

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"

	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
)

func (r *Runner) route(ctx context.Context, target, text string) (definitions.Journey, error) {
	_, span := otel.Tracer("az-agent-platform/runtime").Start(ctx, "hub.route")
	defer span.End()
	if target != "" {
		journey, ok := r.definitions.Journey(target)
		if !ok {
			return definitions.Journey{}, fmt.Errorf("unknown journey target")
		}
		return journey, nil
	}
	if strings.Contains(strings.ToLower(text), "shift") || strings.Contains(strings.ToLower(text), "swap") {
		if journey, ok := r.definitions.Journey("shift-swap"); ok {
			return journey, nil
		}
	}
	journey, ok := r.definitions.DefaultJourney()
	if !ok || strings.TrimSpace(text) == "" {
		return definitions.Journey{}, fmt.Errorf("no journey matches request")
	}
	return journey, nil
}
