package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/microsoft/agent-framework-go/message"

	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
)

// seededHistory is n turns of a user question and an assistant answer, each padded.
func seededHistory(n int) []*message.Message {
	var out []*message.Message
	for i := 0; i < n; i++ {
		tag := string(rune('A' + i))
		out = append(out, userMsg("question-"+tag+" "+padded(400)), assistantText("answer-"+tag+" "+padded(400)))
	}
	return out
}

func (h *harness) runSeeded(history []*message.Message, budget int) (RunOutput, error) {
	h.t.Helper()
	ctx := context.Background()
	if _, err := h.store.Start(ctx, h.owner, "", h.set.Digest()); err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.RecordRunScope(ctx, h.owner, journal.RunScope{ProductVersion: h.version, EndpointID: "endpoint", Settings: json.RawMessage(`{}`)}); err != nil {
		h.t.Fatal(err)
	}
	raw, err := json.Marshal(history)
	if err != nil {
		h.t.Fatal(err)
	}
	h.runner.tenancy.HistoryBudget = budget
	in := RunInput{Store: h.store, Owner: h.owner, History: raw, ThreadID: h.command.ThreadID, RunID: h.command.RunID, Principal: h.command.Principal, Text: h.command.Text, DefinitionDigest: h.set.Digest(), ProductVersion: h.version}
	in.Settings.MaxActiveJourneys = 2
	return h.runner.converse(ctx, in, h.set, h.cat)
}

func (h *harness) truncationEvents() []string {
	h.t.Helper()
	rows, err := h.pool.Query(context.Background(), "SELECT detail::text FROM agent_events WHERE kind='history_truncated' ORDER BY sequence")
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func providerText(r ModelRequest) string {
	var b strings.Builder
	for _, m := range r.ProviderMessages {
		for _, c := range m.Contents {
			if tc, ok := c.(*message.TextContent); ok {
				b.WriteString(tc.Text + "\n")
			}
		}
	}
	return b.String()
}

func TestTrimHistoryInvokeTrimsRequestKeepsPersisted(t *testing.T) {
	h := newHarness(t)
	h.model.script = []func(ModelRequest) ModelResponse{answer("done")}
	history := seededHistory(4)
	// Fits the last two seeded turns plus the admitted "hello", not more.
	budget := estimateTokens(history[4:]) + 50
	out, err := h.runSeeded(history, budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.model.requests) != 1 {
		t.Fatalf("requests = %d", len(h.model.requests))
	}
	seen := providerText(h.model.requests[0])
	for _, gone := range []string{"question-A", "answer-A", "question-B", "answer-B"} {
		if strings.Contains(seen, gone) {
			t.Fatalf("request still has %s:\n%s", gone, seen)
		}
	}
	for _, kept := range []string{"question-C", "answer-D", "hello"} {
		if !strings.Contains(seen, kept) {
			t.Fatalf("request lacks %s:\n%s", kept, seen)
		}
	}
	var persisted []*message.Message
	if err := json.Unmarshal(out.History, &persisted); err != nil {
		t.Fatal(err)
	}
	// 8 seeded + hello + done.
	if len(persisted) != 10 || !strings.Contains(marshalled(t, persisted), "question-A") {
		t.Fatalf("persisted %d messages", len(persisted))
	}
	events := h.truncationEvents()
	if len(events) != 1 || !strings.Contains(events[0], `"turns_dropped": 2`) || !strings.Contains(events[0], `"messages_dropped": 4`) {
		t.Fatalf("events = %v", events)
	}
	// Provenance digests what the model saw (the kept history without the new message).
	kept, _ := trimHistory(append(append([]*message.Message{}, history...), userMsg("hello")), budget, nil)
	want, _ := json.Marshal(kept[:len(kept)-1])
	if string(h.model.requests[0].History) != string(want) {
		t.Fatalf("request history digest input = %s", h.model.requests[0].History)
	}
}

func TestTrimHistoryInvokeZeroBudgetDisablesTrimming(t *testing.T) {
	h := newHarness(t)
	h.model.script = []func(ModelRequest) ModelResponse{answer("done")}
	if _, err := h.runSeeded(seededHistory(4), 0); err != nil {
		t.Fatal(err)
	}
	seen := providerText(h.model.requests[0])
	if !strings.Contains(seen, "question-A") || !strings.Contains(seen, "answer-D") {
		t.Fatalf("trimmed with zero budget:\n%s", seen)
	}
	if events := h.truncationEvents(); len(events) != 0 {
		t.Fatalf("events = %v", events)
	}
}

func TestTrimHistoryInvokeNoEventWhenNothingDropped(t *testing.T) {
	h := newHarness(t)
	h.model.script = []func(ModelRequest) ModelResponse{answer("done")}
	if _, err := h.runSeeded(seededHistory(2), 1<<20); err != nil {
		t.Fatal(err)
	}
	if events := h.truncationEvents(); len(events) != 0 {
		t.Fatalf("events = %v", events)
	}
}

func TestTrimHistoryInvokeOneEventPerTrimmingRequest(t *testing.T) {
	h := newHarness(t)
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "activate_journey", `{"journey":"alpha"}`)),
		answer("done"),
	}
	history := seededHistory(4)
	if _, err := h.runSeeded(history, estimateTokens(history[6:])+50); err != nil {
		t.Fatal(err)
	}
	if len(h.model.requests) != 2 {
		t.Fatalf("requests = %d", len(h.model.requests))
	}
	if events := h.truncationEvents(); len(events) != 2 {
		t.Fatalf("events = %v", events)
	}
}
