package runtime

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/catalog"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journeys"
)

func TestCodeUnknownTargetIsJourneyUnavailable(t *testing.T) {
	h := newHarness(t)
	_, _, err := h.runner.Binding(context.Background(), RunInput{Store: h.store, Owner: h.owner, TargetJourney: "nope"})
	if !errors.Is(err, ErrJourneyUnavailable) || errors.Is(err, ErrPolicyConflict) || errors.Is(err, ErrActivationRefused) {
		t.Fatalf("Binding error = %v", err)
	}
}

func TestCodePolicyConflictTarget(t *testing.T) {
	conflict := unavailableJourney(catalog.Resolution{Unavailable: []catalog.Unavailable{{Reason: catalog.ReasonMissing, Operation: "a"}, {Reason: catalog.ReasonPolicyConflict, Operation: "b"}}})
	if !errors.Is(conflict, ErrPolicyConflict) || !errors.Is(conflict, ErrJourneyUnavailable) || errors.Is(conflict, ErrActivationRefused) {
		t.Fatalf("conflict error = %v", conflict)
	}
	missing := unavailableJourney(catalog.Resolution{Unavailable: []catalog.Unavailable{{Reason: catalog.ReasonMissing, Operation: "a"}}})
	if !errors.Is(missing, ErrJourneyUnavailable) || errors.Is(missing, ErrPolicyConflict) || errors.Is(missing, ErrActivationRefused) {
		t.Fatalf("missing error = %v", missing)
	}
}

func TestCodeActivationRefusedTarget(t *testing.T) {
	h := newHarness(t)
	h.model.script = []func(ModelRequest) ModelResponse{respond(toolCall("c1", "alpha_book", `{}`))}
	if out, err := h.run("alpha", 1); err != nil || !out.Waiting {
		t.Fatalf("setup: %+v %v", out, err)
	}
	h.approvePending()
	_, err := h.runner.converse(context.Background(), RunInput{Store: h.store, Owner: h.owner, TargetJourney: "beta", Settings: settingsWith(1)}, h.set, h.cat)
	if !errors.Is(err, ErrActivationRefused) || !errors.Is(err, journeys.ErrRefused) || !errors.Is(err, ErrJourneyUnavailable) {
		t.Fatalf("converse error = %v", err)
	}
	if active, _ := h.active(); !slices.Equal(active, []string{"alpha"}) {
		t.Fatalf("refusal changed the active set: %v", active)
	}
}
