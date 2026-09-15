package devtui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestModelKeepsPendingInteractionAfterRunFinished(t *testing.T) {
	m := NewModel(Client{}, "thread", "", "journey")
	m.apply(Event{Type: "STATE_SNAPSHOT", Data: []byte(`{"snapshot":{"answer":"previous answer","runState":"awaiting_input","interaction":{"id":"ask-1","kind":"clarification","questions":[{"id":"choice","header":"Destination","question":"Where?","options":[{"label":"A","description":"Alpha"},{"label":"B","description":"Beta"}]}]}}}`)})
	m.apply(Event{Type: "RUN_FINISHED", Data: []byte(`{"type":"RUN_FINISHED"}`)})
	if m.pending == nil || m.status != "awaiting input" {
		t.Fatalf("pending = %#v, status = %q", m.pending, m.status)
	}
	view := m.View().Content
	if strings.Contains(view, "previous answer") || !strings.Contains(view, "Destination") || !strings.Contains(view, "A") {
		t.Fatalf("view = %q", view)
	}
}

func TestModelShowsApprovalAndToolEventsWithoutTerminalEscapes(t *testing.T) {
	m := NewModel(Client{}, "thread", "", "journey")
	m.apply(Event{Type: "interaction.requested", Data: []byte(`{"id":"approval-1","kind":"approval","call":{"callId":"call-1","name":"delete_record","arguments":{"record":"x"},"binding":"bound"}}`)})
	m.apply(Event{Type: "tool.started", Data: []byte(`{"toolName":"delete_record"}`)})
	m.apply(Event{Type: "TEXT_MESSAGE_CONTENT", Data: []byte(`{"delta":"ok\u001b[2J"}`)})
	view := m.View().Content
	if !strings.Contains(view, "delete_record") || !strings.Contains(view, `{"record":"x"}`) || !strings.Contains(view, "tool.started") {
		t.Fatalf("view = %q", view)
	}
	if strings.Contains(strings.Join(m.transcript, "\n"), "\x1b") {
		t.Fatalf("untrusted control sequence was retained: %q", m.transcript)
	}
}

func TestModelInitRetainsResumeStateAndFocus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("event: STATE_SNAPSHOT\ndata: {\"snapshot\":{\"threadId\":\"thread\",\"answer\":\"restored\"}}\n\n"))
	}))
	defer server.Close()
	m := NewModel(Client{BaseURL: server.URL}, "thread", "run", "journey")
	cmd := m.Init()
	if !m.streaming || m.runID != "run" || m.cancel == nil {
		t.Fatalf("resume state lost: %#v", m)
	}
	if view := m.View().Content; !strings.Contains(view, "thread: thread") || !strings.Contains(view, "run: run") {
		t.Fatalf("resume IDs not visible: %q", view)
	}
	_ = cmd
	if !m.input.Focused() {
		t.Fatal("resume input was not focused")
	}
}

func TestResumedModelFocusesInputAndAcceptsClarificationKeypress(t *testing.T) {
	m := NewModel(Client{}, "thread", "run", "journey")
	_ = m.Init()
	if !m.input.Focused() {
		t.Fatal("resumed model did not focus input")
	}
	m.apply(Event{Type: "CUSTOM", Data: []byte(`{"type":"CUSTOM","name":"interaction.requested","value":{"id":"ask-1","kind":"clarification","questions":[{"id":"choice","header":"Choice","question":"Pick","options":[{"label":"A","description":"Alpha"}]}]}}`)})
	_, _ = m.Update(tea.KeyPressMsg(tea.Key{Text: "A"}))
	if m.input.Value() != "A" {
		t.Fatalf("keypress did not reach resumed input: %q", m.input.Value())
	}
}

func TestModelIgnoresDuplicatedSnapshotAnswerAndSanitizesHeader(t *testing.T) {
	m := NewModel(Client{}, "thread\x1b[2J", "run\x1b[2J", "journey\x1b[2J")
	m.apply(Event{Type: "STATE_SNAPSHOT", Data: []byte(`{"snapshot":{"answer":"same"}}`)})
	m.apply(Event{Type: "TEXT_MESSAGE_CONTENT", Data: []byte(`{"delta":"same"}`)})
	if got := strings.Join(m.transcript, "\n"); got != "assistant: same" {
		t.Fatalf("transcript = %q", got)
	}
	header := strings.Split(m.View().Content, "\n")[0]
	if strings.Contains(header, "\x1b") {
		t.Fatalf("header contains terminal control sequence: %q", header)
	}
}

func TestModelRequiresRetryAfterEOFWithoutTerminalEvent(t *testing.T) {
	requests := make(chan Request, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	m := NewModel(Client{BaseURL: server.URL}, "thread", "", "journey")
	request := Request{ThreadID: "thread", RunID: "run", Text: "hello"}
	cmd := m.start(request)
	_, _ = m.Update(cmd())
	if m.terminal || !strings.Contains(m.status, "unexpected EOF") {
		t.Fatalf("terminal = %t status = %q", m.terminal, m.status)
	}
	m.input.SetValue("/retry")
	_, retry := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if m.lastRequest == nil || m.lastRequest.RunID != "run" || retry == nil {
		t.Fatalf("retry did not retain request: %#v", m.lastRequest)
	}
	_ = requests
}

func TestModelKeepsPendingReplyForTransportRetry(t *testing.T) {
	m := NewModel(Client{}, "thread", "", "journey")
	m.apply(Event{Type: "interaction.requested", Data: []byte(`{"id":"approval-1","kind":"approval","call":{"binding":"bound"}}`)})
	m.input.SetValue("approve")
	_, command := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if command == nil || m.pending == nil || m.pending.ID != "approval-1" || m.lastRequest == nil || m.lastRequest.RunID == "" {
		t.Fatalf("pending reply was not retained: pending=%#v request=%#v", m.pending, m.lastRequest)
	}
}

func TestResumedModelRetriesEOFUsingSameRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	m := NewModel(Client{BaseURL: server.URL}, "thread", "run", "journey")
	batch, ok := m.Init()().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("init command = %#v", m.Init()())
	}
	_, _ = m.Update(batch[1]())
	m.input.SetValue("/retry")
	_, retry := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if retry == nil || m.runID != "run" || m.status != "resuming" {
		t.Fatalf("resume retry lost run: run=%q status=%q", m.runID, m.status)
	}
}

func TestModelUpdateCollectsClarificationsFromSnapshot(t *testing.T) {
	m := NewModel(Client{}, "thread", "", "journey")
	m.apply(Event{Type: "STATE_SNAPSHOT", Data: []byte(`{"snapshot":{"runState":"awaiting_input","interaction":{"id":"ask-1","kind":"clarification","questions":[{"id":"first","header":"First","question":"One?","options":[{"label":"A","description":"Alpha"}]},{"id":"second","header":"Second","question":"Two?","options":[{"label":"B","description":"Beta"}]}]}}}`)})
	m.input.SetValue("A")
	_, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if m.pending == nil || m.questionIndex != 1 || m.answers["first"]["option"] != "A" {
		t.Fatalf("pending = %#v index = %d answers = %#v", m.pending, m.questionIndex, m.answers)
	}
}
