package platform

import (
	"errors"
	"fmt"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
)

func TestCodeFailureReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"activation refused", agentruntime.ErrActivationRefused, journal.ReasonActivationRefused},
		{"policy conflict", agentruntime.ErrPolicyConflict, journal.ReasonPolicyConflict},
		{"journey unavailable", agentruntime.ErrJourneyUnavailable, journal.ReasonJourneyUnavailable},
		{"wrapped activation refused", fmt.Errorf("x: %w", agentruntime.ErrActivationRefused), journal.ReasonActivationRefused},
		{"wrapped policy conflict", fmt.Errorf("x: %w", agentruntime.ErrPolicyConflict), journal.ReasonPolicyConflict},
		{"wrapped journey unavailable", fmt.Errorf("x: %w", agentruntime.ErrJourneyUnavailable), journal.ReasonJourneyUnavailable},
		{"plain", errors.New("boom"), ""},
		{"dependency unavailable", agentruntime.ErrDependencyUnavailable, ""},
		{"nil", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := failureReason(c.err); got != c.want {
				t.Fatalf("failureReason = %q, want %q", got, c.want)
			}
		})
	}
}
