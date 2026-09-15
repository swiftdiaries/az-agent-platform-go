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
	if !strings.Contains(view, "previous answer") || !strings.Contains(view, "Destination") || !strings.Contains(view, "A") {
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

func TestModelInitRetainsResumeStateAndAppliesStreamThroughUpdate(t *testing.T) {
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
	updated, next := m.Update(cmd())
	if updated != m || !strings.Contains(strings.Join(m.transcript, "\n"), "restored") {
		t.Fatalf("updated = %#v, transcript = %#v", updated, m.transcript)
	}
	_, _ = m.Update(next())
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
