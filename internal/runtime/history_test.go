package runtime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/microsoft/agent-framework-go/message"
)

func userMsg(text string) *message.Message {
	return &message.Message{Role: message.RoleUser, Contents: message.Contents{&message.TextContent{Text: text}}}
}

func assistantText(text string) *message.Message {
	return &message.Message{Role: message.RoleAssistant, Contents: message.Contents{&message.TextContent{Text: text}}}
}

func assistantCall(id string) *message.Message {
	return &message.Message{Role: message.RoleAssistant, Contents: message.Contents{&message.FunctionCallContent{CallID: id, Name: "lookup", Arguments: `{}`}}}
}

// turnOfText builds a turn: user, assistant (padded to make the turn weigh something).
func padded(n int) string { return strings.Repeat("x", n) }

func fourTurns() []*message.Message {
	return []*message.Message{
		userMsg("t1 " + padded(400)), assistantText("a1 " + padded(400)),
		userMsg("t2 " + padded(400)), assistantCall("c2"), resultMessage("c2", "ok"), assistantText("a2"),
		userMsg("t3 " + padded(400)), assistantText("a3 " + padded(400)),
		userMsg("t4 short"), assistantText("a4"),
	}
}

func marshalled(t *testing.T, h []*message.Message) string {
	t.Helper()
	data, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestTrimHistoryNoopUnderBudgetAndZero(t *testing.T) {
	h := fourTurns()
	for _, budget := range []int{0, -5, 1 << 20} {
		got, tr := trimHistory(h, budget, nil)
		if tr != (Truncation{}) || marshalled(t, got) != marshalled(t, h) {
			t.Fatalf("budget %d: %+v", budget, tr)
		}
		if len(got) > 0 && &got[0] == &h[0] {
			t.Fatalf("budget %d: result aliases input", budget)
		}
	}
}

func TestTrimHistoryDropsOldestTurns(t *testing.T) {
	h := fourTurns()
	full := estimateTokens(h)
	// Fits only after dropping turn 1 and turn 2.
	budget := estimateTokens(h[6:]) + 1
	got, tr := trimHistory(h, budget, nil)
	if len(got) != 4 || got[0] != h[6] {
		t.Fatalf("kept %d messages", len(got))
	}
	if tr.TurnsDropped != 2 || tr.MessagesDropped != 6 || tr.EstimateBefore != full || tr.EstimateAfter != estimateTokens(got) || tr.Budget != budget {
		t.Fatalf("truncation = %+v (full %d)", tr, full)
	}
	if tr.EstimateAfter > budget {
		t.Fatalf("still over budget: %+v", tr)
	}
	// Drops no more than needed: budget that fits turns 2-4 drops only turn 1.
	got, tr = trimHistory(h, estimateTokens(h[2:]), nil)
	if tr.TurnsDropped != 1 || tr.MessagesDropped != 2 || len(got) != 8 {
		t.Fatalf("one turn: %+v len=%d", tr, len(got))
	}
}

func TestTrimHistoryKeepsLatestTurnOverBudget(t *testing.T) {
	h := fourTurns()
	got, tr := trimHistory(h, 1, nil)
	if len(got) != 2 || got[0] != h[8] || tr.TurnsDropped != 3 || tr.EstimateAfter <= tr.Budget {
		t.Fatalf("got %d, %+v", len(got), tr)
	}
}

func TestTrimHistoryKeepsProtectedTurn(t *testing.T) {
	h := fourTurns()
	got, tr := trimHistory(h, 1, map[string]bool{"c2": true})
	// Turn 2 (protected) and turn 4 (latest) stay; turns 1 and 3 drop.
	if tr.TurnsDropped != 2 || len(got) != 6 || got[0] != h[2] || got[5] != h[9] {
		t.Fatalf("got %d, %+v", len(got), tr)
	}
	if got[3] != h[5] || got[4] != h[8] {
		t.Fatalf("order not preserved")
	}
}

func pairsIntact(t *testing.T, h []*message.Message) {
	t.Helper()
	calls, results := map[string]bool{}, map[string]bool{}
	for _, m := range h {
		for _, c := range m.Contents {
			switch content := c.(type) {
			case *message.FunctionCallContent:
				calls[content.CallID] = true
			case *message.FunctionResultContent:
				results[content.CallID] = true
			}
		}
	}
	if !reflect.DeepEqual(calls, results) {
		t.Fatalf("calls %v results %v", calls, results)
	}
}

func TestTrimHistoryNeverSplitsPairs(t *testing.T) {
	h := fourTurns()
	for budget := 1; budget < estimateTokens(h)+2; budget += 7 {
		got, _ := trimHistory(h, budget, nil)
		pairsIntact(t, got)
	}
	// A result in a later turn than its call keeps both turns as one unit.
	split := []*message.Message{
		userMsg("a " + padded(400)), assistantCall("x"),
		userMsg("b " + padded(400)), resultMessage("x", "ok"), assistantText("done"),
		userMsg("c " + padded(400)), assistantText("ok"),
		userMsg("latest"), assistantText("ok"),
	}
	for budget := 1; budget < estimateTokens(split)+2; budget += 5 {
		got, _ := trimHistory(split, budget, nil)
		pairsIntact(t, got)
	}
	got, tr := trimHistory(split, estimateTokens(split[5:])+1, nil)
	if tr.TurnsDropped != 2 || tr.MessagesDropped != 5 || len(got) != 4 {
		t.Fatalf("unit drop: %d %+v", len(got), tr)
	}
	// Protecting the call's turn keeps the result's turn too.
	got, _ = trimHistory(split, 1, map[string]bool{"x": true})
	pairsIntact(t, got)
	if len(got) != 7 {
		t.Fatalf("protected unit kept %d", len(got))
	}
}

func TestTrimHistoryLeadingMessagesFormATurn(t *testing.T) {
	h := []*message.Message{assistantText("lead " + padded(400)), userMsg("u"), assistantText("a")}
	got, tr := trimHistory(h, 1, nil)
	if tr.TurnsDropped != 1 || len(got) != 2 || got[0] != h[1] {
		t.Fatalf("got %d %+v", len(got), tr)
	}
}

func TestTrimHistoryDeterministicAndPure(t *testing.T) {
	h := fourTurns()
	before := marshalled(t, h)
	n := len(h)
	budget := estimateTokens(h[6:]) + 1
	a, ta := trimHistory(h, budget, map[string]bool{"c2": true})
	b, tb := trimHistory(h, budget, map[string]bool{"c2": true})
	if marshalled(t, a) != marshalled(t, b) || ta != tb {
		t.Fatal("not deterministic")
	}
	if len(h) != n || marshalled(t, h) != before {
		t.Fatal("input modified")
	}
	a[0] = nil
	if h[0] == nil {
		t.Fatal("result aliases input")
	}
}
