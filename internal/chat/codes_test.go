package chat

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/platform"
)

func TestCodeFailureCode(t *testing.T) {
	cases := map[string]string{
		journal.ReasonJourneyUnavailable: journal.ReasonJourneyUnavailable,
		journal.ReasonPolicyConflict:     journal.ReasonPolicyConflict,
		journal.ReasonActivationRefused:  journal.ReasonActivationRefused,
		journal.ReasonInteractionExpired: journal.ReasonInteractionExpired,
		"":                               "internal_error",
		"failed":                         "internal_error",
		"bogus":                          "internal_error",
	}
	for in, want := range cases {
		if got := failureCode(in); got != want {
			t.Errorf("failureCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExpiredReplyStatus(t *testing.T) {
	for name, err := range map[string]error{
		"direct":  platform.ErrExpired,
		"wrapped": fmt.Errorf("reply: %w", platform.ErrExpired),
	} {
		w := httptest.NewRecorder()
		commandError(w, err)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "interaction_expired") {
			t.Errorf("%s: got %d %q", name, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	commandError(w, platform.ErrConflict)
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "interaction_expired") {
		t.Errorf("conflict: got %d %q", w.Code, w.Body.String())
	}
}
