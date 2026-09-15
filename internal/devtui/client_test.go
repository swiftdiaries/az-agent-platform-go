package devtui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	aguievents "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
)

func TestClientStreamsAGUIRequest(t *testing.T) {
	var got chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("request = %s authorization = %q", r.Method, r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: TEXT_MESSAGE_CONTENT\ndata: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"delta\":\"hello\"}\n\nevent: RUN_FINISHED\ndata: {\"type\":\"RUN_FINISHED\"}\n\n"))
	}))
	defer server.Close()

	var events []Event
	err := (Client{BaseURL: server.URL, Token: "token"}).Stream(context.Background(), Request{
		ThreadID: "thread", RunID: "run", JourneyID: "support", Text: "help",
	}, func(event Event) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != "TEXT_MESSAGE_CONTENT" || string(events[0].Data) != `{"type":"TEXT_MESSAGE_CONTENT","delta":"hello"}` {
		t.Fatalf("events = %#v", events)
	}
	if got.ThreadID != "thread" || got.RunID != "run" || got.Messages[0].Content != "help" || got.ForwardedProps.JourneyID != "support" {
		t.Fatalf("request = %#v", got)
	}
}

func TestClientResumesWithBothIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("threadId") != "thread" || r.URL.Query().Get("runId") != "run" {
			t.Fatalf("request = %s %s", r.Method, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte("event: RUN_FINISHED\ndata: {\"type\":\"RUN_FINISHED\"}\n\n"))
	}))
	defer server.Close()

	var events []Event
	err := (Client{BaseURL: server.URL, Token: "token"}).Resume(context.Background(), "thread", "run", func(event Event) { events = append(events, event) })
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %#v, err = %v", events, err)
	}
}

func TestClientSendsEmptyMessagesForInteractionReply(t *testing.T) {
	var got chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("event: RUN_FINISHED\ndata: {\"type\":\"RUN_FINISHED\"}\n\n"))
	}))
	defer server.Close()
	err := (Client{BaseURL: server.URL}).Stream(context.Background(), Request{
		ThreadID: "thread", RunID: "reply", Reply: &InteractionReply{InteractionID: "pending", Kind: "approval", Answer: map[string]string{"decision": "deny", "binding": "exact"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 0 || got.ForwardedProps.InteractionReply == nil {
		t.Fatalf("request = %#v", got)
	}
}

func TestClientReportsHTTPErrorAndCancelsStream(t *testing.T) {
	t.Run("error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusForbidden)
		}))
		defer server.Close()
		err := (Client{BaseURL: server.URL, Token: "token"}).Resume(context.Background(), "thread", "run", nil)
		if err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		started := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done()
		}))
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- (Client{BaseURL: server.URL, Token: "token"}).Resume(ctx, "thread", "run", nil) }()
		<-started
		cancel()
		if err := <-done; err == nil {
			t.Fatal("Stream returned nil after context cancellation")
		}
	})
}

func TestClientEmitsEventBeforeStreamEOF(t *testing.T) {
	release := make(chan struct{})
	seen := make(chan Event, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: TEXT_MESSAGE_CONTENT\ndata: {\"delta\":\"first\"}\n\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("event: RUN_FINISHED\ndata: {\"type\":\"RUN_FINISHED\"}\n\n"))
	}))
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		done <- (Client{BaseURL: server.URL}).Resume(context.Background(), "thread", "run", func(event Event) { seen <- event })
	}()
	select {
	case event := <-seen:
		if event.Type != "TEXT_MESSAGE_CONTENT" {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("event was held until EOF")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestParseSSEBoundsAggregateEvent(t *testing.T) {
	data := strings.Repeat("x", maxSSEEvent/2+1)
	err := parseSSE(strings.NewReader("data: "+data+"\ndata: "+data+"\n\n"), nil)
	if err == nil {
		t.Fatal("accepted an overlong aggregate SSE event")
	}
}

func TestClientDeliversSDKCustomInteractionEvent(t *testing.T) {
	payload, err := aguievents.NewCustomEvent("interaction.requested", aguievents.WithValue(map[string]any{
		"id": "ask-1", "kind": "approval", "call": map[string]any{"name": "delete_record", "arguments": map[string]any{"record": "x"}},
	})).ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "event: CUSTOM\ndata: %s\n\nevent: RUN_FINISHED\ndata: {\"type\":\"RUN_FINISHED\"}\n\n", payload)
	}))
	defer server.Close()
	var received Event
	err = (Client{BaseURL: server.URL}).Resume(context.Background(), "thread", "run", func(event Event) {
		if event.Type == "CUSTOM" {
			received = event
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	m := NewModel(Client{}, "thread", "", "journey")
	m.apply(received)
	if m.pending == nil || m.pending.ID != "ask-1" || m.pending.Call.Name != "delete_record" {
		t.Fatalf("custom interaction was not applied: %#v", m.pending)
	}
}

func TestClientRejectsRedirectBeforeSendingBearerTokenElsewhere(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("redirect target received authorization %q", r.Header.Get("Authorization"))
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()
	err := (Client{BaseURL: server.URL, Token: "secret"}).Resume(context.Background(), "thread", "run", nil)
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("err = %v", err)
	}
}
