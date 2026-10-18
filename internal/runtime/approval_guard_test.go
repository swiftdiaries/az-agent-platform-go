package runtime

import (
	"context"
	"slices"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/catalog"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
)

// bookTool is an approval-gated tool that both journeys can offer under one alias.
var bookTool = catalog.Tool{Alias: "alpha_book", OperationKey: "alpha.book", RemoteName: "book", Class: definitions.ClassEffectful, RequiresApproval: true}

// waitingApproval runs alpha_book to its approval wait and returns the interaction ID.
func waitingApproval(t *testing.T, h *harness, call string) string {
	t.Helper()
	h.model.script = append(h.model.script, respond(toolCall("c1", "alpha_book", call)))
	out, err := h.run("alpha", 2)
	if err != nil || !out.Waiting {
		t.Fatalf("setup: %+v %v", out, err)
	}
	return pendingApproval(t, h)
}

func pendingApproval(t *testing.T, h *harness) string {
	t.Helper()
	var id string
	if err := h.pool.QueryRow(context.Background(), "SELECT id FROM agent_interactions WHERE thread_id='thread' AND kind='approval' AND state='pending'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func marker(t *testing.T, h *harness, id string) string {
	t.Helper()
	var m string
	if err := h.pool.QueryRow(context.Background(), "SELECT marker FROM agent_interactions WHERE id=$1", id).Scan(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestApprovalGuardJourneyBound(t *testing.T) {
	h := newHarness(t)
	h.cat.journeys["beta"] = append(h.cat.journeys["beta"], bookTool)
	first := waitingApproval(t, h, `{"n":1}`)
	h.approvePending()
	// alpha no longer resolves; beta, active and most recently used, now owns the alias.
	h.cat.unavailable = map[string]bool{"alpha": true}
	if err := h.store.SetActiveJourneys(context.Background(), h.owner, []string{"alpha", "beta"}); err != nil {
		t.Fatal(err)
	}
	h.model.script = append(h.model.script, answer("done"))
	out, err := h.run("", 2)
	if err != nil || out.Answer != "done" || out.Waiting {
		t.Fatalf("continuation: %+v %v", out, err)
	}
	if got := h.cat.called(); len(got) != 0 {
		t.Fatalf("dispatched = %v", got)
	}
	if got := marker(t, h, first); got != "superseded" {
		t.Fatalf("marker = %q", got)
	}
	if got := asJSON(t, historyResults(t, out.History)["c1"]); got != `{"error":"approval_superseded","message":"the approved action is no longer available"}` {
		t.Fatalf("c1 = %s", got)
	}
}

func TestApprovalGuardBindingMismatchOpensNewWait(t *testing.T) {
	h := newHarness(t)
	first := waitingApproval(t, h, `{"n":1}`)
	var firstCall, firstBinding string
	if err := h.pool.QueryRow(context.Background(), "SELECT artifact->'call'->>'callId',artifact->'call'->>'binding' FROM agent_interactions WHERE id=$1", first).Scan(&firstCall, &firstBinding); err != nil {
		t.Fatal(err)
	}
	h.approvePending()
	h.version = "v2" // the tenant moved to another product version
	out, err := h.run("alpha", 2)
	if err != nil || !out.Waiting {
		t.Fatalf("continuation: %+v %v", out, err)
	}
	if got := h.cat.called(); len(got) != 0 {
		t.Fatalf("dispatched = %v", got)
	}
	if got := marker(t, h, first); got != "superseded" {
		t.Fatalf("marker = %q", got)
	}
	second := pendingApproval(t, h)
	var journey, callID, binding, provider string
	if err := h.pool.QueryRow(context.Background(), "SELECT journey_id,artifact->'call'->>'callId',artifact->'call'->>'binding',artifact->'call'->>'providerCallId' FROM agent_interactions WHERE id=$1", second).Scan(&journey, &callID, &binding, &provider); err != nil {
		t.Fatal(err)
	}
	if second == first || journey != "alpha" || callID == firstCall || binding == firstBinding || provider != "c1" {
		t.Fatalf("new wait = %s/%s/%s/%s", second, journey, callID, provider)
	}
	// Approving the new interaction dispatches exactly once.
	h.approveAs("run-approve-2", "approve-2")
	h.model.script = append(h.model.script, answer("booked"))
	done, err := h.run("alpha", 2)
	if err != nil || done.Answer != "booked" {
		t.Fatalf("second continuation: %+v %v", done, err)
	}
	if got := h.cat.called(); !slices.Equal(got, []string{"alpha_book"}) {
		t.Fatalf("dispatched = %v", got)
	}
	if got := marker(t, h, second); got != "dispatched" {
		t.Fatalf("marker = %q", got)
	}
}

func TestApprovalGuardMatchDispatchesOnce(t *testing.T) {
	h := newHarness(t)
	h.cat.journeys["beta"] = append(h.cat.journeys["beta"], bookTool)
	first := waitingApproval(t, h, `{"n":1}`)
	h.approvePending()
	// beta is the most recently used journey and offers the same alias.
	if err := h.store.SetActiveJourneys(context.Background(), h.owner, []string{"alpha", "beta"}); err != nil {
		t.Fatal(err)
	}
	h.model.script = append(h.model.script, answer("booked"))
	out, err := h.run("", 2)
	if err != nil || out.Answer != "booked" {
		t.Fatalf("continuation: %+v %v", out, err)
	}
	if got := h.cat.called(); !slices.Equal(got, []string{"alpha_book"}) {
		t.Fatalf("dispatched = %v", got)
	}
	var journey string
	if err := h.pool.QueryRow(context.Background(), "SELECT journey_id FROM agent_operations WHERE thread_id='thread'").Scan(&journey); err != nil || journey != "alpha" {
		t.Fatalf("operation journey = %q/%v", journey, err)
	}
	if got := marker(t, h, first); got != "dispatched" {
		t.Fatalf("marker = %q", got)
	}
}
