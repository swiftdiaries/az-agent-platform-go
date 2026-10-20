package runtime

import (
	"context"
	"slices"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/catalog"
)

func attemptOutcomes(t *testing.T, h *harness) []string {
	t.Helper()
	rows, err := h.pool.Query(context.Background(), "SELECT outcome FROM agent_attempts ORDER BY attempt")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			t.Fatal(err)
		}
		out = append(out, o)
	}
	return out
}

func TestGuardedCallEffectfulRejectedSentOnce(t *testing.T) {
	h := newHarness(t)
	h.cat.failures = map[string]error{"alpha_book": catalog.ErrBusinessRejected}
	waitingApproval(t, h, `{"n":1}`)
	h.approvePending()
	h.model.script = append(h.model.script, answer("sorry"))
	out, err := h.run("alpha", 2)
	if err != nil || out.Answer != "sorry" {
		t.Fatalf("continuation: %+v %v", out, err)
	}
	if got := h.cat.called(); !slices.Equal(got, []string{"alpha_book"}) {
		t.Fatalf("an approved effectful call was re-sent after rejection: %v", got)
	}
	if got := asJSON(t, historyResults(t, out.History)["c1"]); got != `{"error":"business_rejected"}` {
		t.Fatalf("c1 = %s", got)
	}
	if got := attemptOutcomes(t, h); !slices.Equal(got, []string{"rejected"}) {
		t.Fatalf("attempts = %v", got)
	}
}

func TestGuardedCallReadOnlyRejectedRepeatedOnce(t *testing.T) {
	h := newHarness(t)
	h.cat.failures = map[string]error{"alpha_lookup": catalog.ErrBusinessRejected}
	h.model.script = append(h.model.script, respond(toolCall("c1", "alpha_lookup", `{}`)), answer("sorry"))
	out, err := h.run("alpha", 2)
	if err != nil || out.Answer != "sorry" {
		t.Fatalf("run: %+v %v", out, err)
	}
	if got := h.cat.called(); !slices.Equal(got, []string{"alpha_lookup", "alpha_lookup"}) {
		t.Fatalf("read_only calls = %v, want exactly two", got)
	}
	if got := attemptOutcomes(t, h); !slices.Equal(got, []string{"rejected", "rejected"}) {
		t.Fatalf("attempts = %v", got)
	}
}
