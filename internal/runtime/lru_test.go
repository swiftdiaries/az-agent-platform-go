package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/catalog"
)

// countActiveWrites installs a trigger that counts every change of the persisted
// active set, and returns a reader of the count.
func countActiveWrites(t *testing.T, h *harness) func() int {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		"CREATE TABLE lru_writes(n int)",
		"CREATE FUNCTION lru_count() RETURNS trigger AS $$ BEGIN INSERT INTO lru_writes VALUES (1); RETURN NEW; END $$ LANGUAGE plpgsql",
		"CREATE TRIGGER lru_trigger AFTER UPDATE OF active_journeys ON agent_conversations FOR EACH ROW EXECUTE FUNCTION lru_count()",
	} {
		if _, err := h.pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	return func() int {
		var n int
		if err := h.pool.QueryRow(ctx, "SELECT count(*) FROM lru_writes").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
}

// storedActive reads the persisted order without the run fence (a waiting run has released it).
func storedActive(t *testing.T, h *harness) []string {
	t.Helper()
	var raw []byte
	if err := h.pool.QueryRow(context.Background(), "SELECT active_journeys FROM agent_conversations WHERE id='thread'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var active []string
	if err := json.Unmarshal(raw, &active); err != nil {
		t.Fatal(err)
	}
	return active
}

func seedActive(t *testing.T, h *harness, ids ...string) {
	t.Helper()
	if err := h.store.SetActiveJourneys(context.Background(), h.owner, ids); err != nil {
		t.Fatal(err)
	}
}

func TestLRUTouchAliasDispatchProtectsFromEviction(t *testing.T) {
	h := newHarness(t)
	seedActive(t, h, "alpha", "beta")
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "alpha_lookup", `{}`)),
		respond(toolCall("c2", "activate_journey", `{"journey":"gamma"}`)),
		answer("done"),
	}
	out, err := h.run("", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := asJSON(t, historyResults(t, out.History)["c2"]); got != `{"activated":"gamma","evicted":["beta"]}` {
		t.Fatalf("c2 = %s", got)
	}
	if active, _ := h.active(); !slices.Equal(active, []string{"alpha", "gamma"}) {
		t.Fatalf("persisted active = %v", active)
	}
}

func TestLRUTouchPersistsOncePerRound(t *testing.T) {
	h := newHarness(t)
	seedActive(t, h, "alpha", "beta")
	writes := countActiveWrites(t, h)
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "beta_lookup", `{}`), toolCall("c2", "alpha_lookup", `{}`), toolCall("c3", "alpha_lookup", `{}`)),
		func(ModelRequest) ModelResponse {
			if active, _ := h.active(); !slices.Equal(active, []string{"beta", "alpha"}) {
				t.Errorf("order persisted at the end of the round = %v", active)
			}
			if n := writes(); n != 1 {
				t.Errorf("writes after the round = %d", n)
			}
			return ModelResponse{Text: "done"}
		},
	}
	if _, err := h.run("", 2); err != nil {
		t.Fatal(err)
	}
	if n := writes(); n != 1 {
		t.Fatalf("writes = %d, want 1", n)
	}
}

func TestLRUTouchNoWriteWhenOrderUnchanged(t *testing.T) {
	h := newHarness(t)
	seedActive(t, h, "alpha", "beta")
	writes := countActiveWrites(t, h)
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "alpha_lookup", `{}`), toolCall("c2", "beta_lookup", `{}`)), // ends as it began
		answer("done"),
	}
	if _, err := h.run("", 2); err != nil {
		t.Fatal(err)
	}
	if n := writes(); n != 0 {
		t.Fatalf("writes = %d, want 0", n)
	}
}

func TestLRUTouchFlushesBeforeApprovalWait(t *testing.T) {
	h := newHarness(t)
	seedActive(t, h, "alpha", "beta")
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "alpha_lookup", `{}`), toolCall("c2", "alpha_book", `{"n":1}`)),
	}
	out, err := h.run("", 2)
	if err != nil || !out.Waiting {
		t.Fatalf("output/error = %+v/%v", out, err)
	}
	if active := storedActive(t, h); !slices.Equal(active, []string{"beta", "alpha"}) {
		t.Fatalf("persisted active = %v", active)
	}
}

func TestLRUTouchApprovedContinuationDispatch(t *testing.T) {
	h := newHarness(t)
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "activate_journey", `{"journey":"alpha"}`)),
		respond(toolCall("c2", "activate_journey", `{"journey":"beta"}`)),
		respond(toolCall("c3", "alpha_book", `{"n":1}`)),
	}
	if out, err := h.run("", 2); err != nil || !out.Waiting {
		t.Fatalf("output/error = %+v/%v", out, err)
	}
	if active := storedActive(t, h); !slices.Equal(active, []string{"alpha", "beta"}) {
		t.Fatalf("active before approval = %v", active)
	}
	h.approvePending()
	h.model.script = append(h.model.script, answer("booked"))
	if done, err := h.run("alpha", 2); err != nil || done.Answer != "booked" {
		t.Fatalf("resume: %+v %v", done, err)
	}
	if got := h.cat.called(); !slices.Equal(got, []string{"alpha_book"}) {
		t.Fatalf("dispatched = %v", got)
	}
	if active, _ := h.active(); !slices.Equal(active, []string{"beta", "alpha"}) {
		t.Fatalf("persisted active = %v", active)
	}
}

func TestLRUTouchReadSkillResourceSuccessOnly(t *testing.T) {
	h := newHarness(t)
	seedActive(t, h, "alpha", "beta")
	writes := countActiveWrites(t, h)
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "read_skill_resource", `{"journey":"alpha","resource":"missing.md"}`), toolCall("c2", "read_skill_resource", `{"journey":"beta","resource":"guide.md"}`), toolCall("c3", "read_skill_resource", `{"journey":"alpha"}`)),
		func(ModelRequest) ModelResponse {
			if n := writes(); n != 0 {
				t.Errorf("writes after refused reads = %d", n)
			}
			return ModelResponse{ToolCalls: []ToolCall{toolCall("c4", "read_skill_resource", `{"journey":"alpha","resource":"guide.md"}`)}}
		},
		answer("done"),
	}
	if _, err := h.run("", 2); err != nil {
		t.Fatal(err)
	}
	if active, _ := h.active(); !slices.Equal(active, []string{"beta", "alpha"}) {
		t.Fatalf("persisted active = %v", active)
	}
	if n := writes(); n != 1 {
		t.Fatalf("writes = %d, want 1", n)
	}
}

func TestLRUTouchInactiveAndNotExecutedTouchNothing(t *testing.T) {
	h := newHarness(t)
	seedActive(t, h, "alpha", "beta")
	writes := countActiveWrites(t, h)
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "gamma_lookup", `{}`)),
		respond(toolCall("c2", "activate_journey", `{"journey":"alpha"}`), toolCall("c3", "beta_lookup", `{}`)),
		answer("done"),
	}
	out, err := h.run("", 2)
	if err != nil {
		t.Fatal(err)
	}
	results := historyResults(t, out.History)
	if got := asJSON(t, results["c1"]); got != `{"error":"tool_not_active"}` {
		t.Fatalf("c1 = %s", got)
	}
	if got := asJSON(t, results["c3"]); got != `{"error":"not_executed","message":"not executed; call again after activation"}` {
		t.Fatalf("c3 = %s", got)
	}
	// Only the activation of the already-active alpha writes (it moves last); the
	// inactive and not-executed calls touch nothing.
	if active, _ := h.active(); !slices.Equal(active, []string{"beta", "alpha"}) {
		t.Fatalf("persisted active = %v", active)
	}
	if n := writes(); n != 1 {
		t.Fatalf("writes = %d, want 1 (the activation)", n)
	}
	if got := h.cat.called(); len(got) != 0 {
		t.Fatalf("dispatched = %v", got)
	}
}

func TestNotPermittedIsAResultNotAFailure(t *testing.T) {
	h := newHarness(t)
	h.cat.failures = map[string]error{"alpha_lookup": fmt.Errorf("call MCP tool: %w", catalog.ErrNotPermitted)}
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "activate_journey", `{"journey":"alpha"}`)),
		respond(toolCall("c2", "alpha_lookup", `{}`)),
		func(request ModelRequest) ModelResponse {
			h.cat.mu.Lock()
			delete(h.cat.failures, "alpha_lookup")
			h.cat.mu.Unlock()
			return respond(toolCall("c3", "alpha_lookup", `{}`))(request)
		},
		answer("done"),
	}
	out, err := h.run("", 2)
	if err != nil || out.Answer != "done" {
		t.Fatalf("output/error = %+v/%v", out, err)
	}
	results := historyResults(t, out.History)
	if got := asJSON(t, results["c2"]); got != `{"error":"not_permitted"}` {
		t.Fatalf("c2 = %s", got)
	}
	if got := asJSON(t, results["c3"]); got != `{"alias":"alpha_lookup","status":"ok"}` {
		t.Fatalf("c3 = %s", got)
	}
	if got := h.cat.called(); !slices.Equal(got, []string{"alpha_lookup", "alpha_lookup"}) {
		t.Fatalf("calls = %v (one per proposal, no retry)", got)
	}
	rows, err := h.pool.Query(context.Background(), "SELECT outcome FROM agent_operations WHERE thread_id='thread' ORDER BY outcome")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var outcomes []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			t.Fatal(err)
		}
		outcomes = append(outcomes, o)
	}
	if !slices.Equal(outcomes, []string{"completed", "rejected"}) {
		t.Fatalf("outcomes = %v", outcomes)
	}
}

func TestNotPermittedDoesNotSpendRepairBudget(t *testing.T) {
	h := newHarness(t)
	h.cat.failures = map[string]error{"alpha_lookup": catalog.ErrNotPermitted}
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "activate_journey", `{"journey":"alpha"}`)),
		respond(toolCall("c2", "alpha_lookup", `{}`)),
		respond(toolCall("c3", "alpha_lookup", `{}`)),
		respond(toolCall("c4", "alpha_lookup", `{}`)),
		answer("done"),
	}
	out, err := h.run("", 2)
	if err != nil || out.Answer != "done" {
		t.Fatalf("output/error = %+v/%v", out, err)
	}
}

func TestNotPermittedAuthRequiredStillFailsRun(t *testing.T) {
	h := newHarness(t)
	h.cat.failures = map[string]error{"alpha_lookup": catalog.ErrAuthRequired}
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "activate_journey", `{"journey":"alpha"}`)),
		respond(toolCall("c2", "alpha_lookup", `{}`)),
	}
	_, err := h.run("", 2)
	if !errors.Is(err, catalog.ErrAuthRequired) {
		t.Fatalf("error = %v, want auth_required", err)
	}
	if got := h.cat.called(); len(got) != 1 {
		t.Fatalf("calls = %v", got)
	}
}
