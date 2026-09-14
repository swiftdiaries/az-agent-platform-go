package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/swiftdiaries/az-agent-platform-go/internal/platform"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

type handler struct {
	auth     Authenticator
	platform *platform.Platform
	ids      IdentityMap
}

func NewHandler(auth Authenticator, service *platform.Platform, pool *pgxpool.Pool) http.Handler {
	return &handler{auth: auth, platform: service, ids: IdentityMap{pool: pool}}
}
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	principal, err := h.auth.Authenticate(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	cursor := int64(0)
	if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		cursor, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || cursor < 0 {
			http.Error(w, "invalid Last-Event-ID", http.StatusBadRequest)
			return
		}
	}
	var input types.RunAgentInput
	create := r.Method == http.MethodPost
	switch r.Method {
	case http.MethodPost:
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		if err := decoder.Decode(&input); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if len(input.Tools) > 0 {
			http.Error(w, "client tools are not supported", http.StatusBadRequest)
			return
		}
	case http.MethodGet:
		input.ThreadID = r.URL.Query().Get("threadId")
		input.RunID = r.URL.Query().Get("runId")
	default:
		w.Header().Set("Allow", "POST, GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if input.ThreadID == "" || input.RunID == "" {
		http.Error(w, "threadId and runId are required", http.StatusBadRequest)
		return
	}
	text, target := "", ""
	if create {
		text, err = lastUserText(input.Messages)
		if err != nil {
			http.Error(w, "user text is required", http.StatusBadRequest)
			return
		}
		target, err = targetJourney(input.ForwardedProps)
		if err != nil {
			http.Error(w, "invalid journey target", http.StatusBadRequest)
			return
		}
	}
	thread, run, communication, err := h.ids.resolve(r.Context(), principal, input.ThreadID, input.RunID, create)
	if err != nil {
		commandError(w, err)
		return
	}
	ctx, span := otel.Tracer("az-agent-platform/chat").Start(r.Context(), "chat.accept")
	defer span.End()
	span.SetAttributes(attribute.String("thread.id", thread), attribute.String("run.id", run))
	if create {
		// Submit synchronously selects only the journey's configured MCP header names;
		// no inbound header map is retained by execution or written to persistence.
		_, err = h.platform.Submit(ctx, platform.Submission{Command: platform.Command{ThreadID: thread, RunID: run, CommunicationID: communication, Principal: principal, Text: text, TargetJourney: target}, Headers: r.Header})
		if err != nil {
			commandError(w, err)
			return
		}
	}
	observation, err := h.platform.Observe(ctx, thread, principal, cursor)
	if err != nil {
		commandError(w, err)
		return
	}
	defer observation.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	emit := func(event events.Event, sequence int64) bool {
		payload, err := json.Marshal(event)
		if err != nil {
			return false
		}
		if sequence > 0 {
			if _, err = fmt.Fprintf(w, "id: %d\n", sequence); err != nil {
				return false
			}
		}
		if _, err = fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return true
	}
	if !emit(events.NewRunStartedEvent(input.ThreadID, input.RunID), 0) {
		return
	}
	snapshot := observation.Snapshot
	var current platform.Run
	for _, candidate := range snapshot.Runs {
		if candidate.RunID == run || slices.Contains(candidate.CommandRunIDs, run) {
			run = candidate.RunID
			current = candidate
			break
		}
	}
	if !emit(events.NewStateSnapshotEvent(map[string]any{"threadId": input.ThreadID, "watermark": snapshot.Watermark, "journeyId": current.JourneyID, "runState": current.State, "pendingCommands": current.PendingCommands, "commands": current.CommandOutcomes, "answer": current.Answer}), max(cursor, snapshot.Watermark)) {
		return
	}
	terminal := func(e platform.Event, sequence int64) bool {
		switch e.Type {
		case "message.completed":
			messageID := input.RunID + ":answer"
			return !emit(events.NewTextMessageStartEvent(messageID, events.WithRole("assistant")), 0) || !emit(events.NewTextMessageContentEvent(messageID, e.Answer), sequence) || !emit(events.NewTextMessageEndEvent(messageID), 0)
		case "run.completed":
			emit(events.NewRunFinishedEvent(input.ThreadID, input.RunID), sequence)
			return true
		case "run.failed", "auth_required", "run.interrupted":
			code := "internal_error"
			if e.Type == "run.interrupted" {
				code = "interrupted"
			}
			if e.Type == "auth_required" {
				code = "auth_required"
			}
			emit(events.NewRunErrorEvent("run could not complete", events.WithErrorCode(code), events.WithRunID(input.RunID)), sequence)
			return true
		default:
			return !emit(events.NewCustomEvent(e.Type, events.WithValue(map[string]any{"communicationId": e.CommunicationID, "reason": e.Reason})), sequence)
		}
	}
	// Project completed state directly from Agent run records, not replay events.
	if current.Answer != "" {
		if terminal(platform.Event{Type: "message.completed", Answer: current.Answer}, 0) {
			return
		}
	}
	switch current.State {
	case platform.RunCompleted:
		terminal(platform.Event{Type: "run.completed"}, 0)
		return
	case platform.RunInterrupted:
		terminal(platform.Event{Type: "run.interrupted"}, 0)
		return
	case platform.RunFailed:
		terminal(platform.Event{Type: "run.failed"}, 0)
		return
	case platform.RunAuthRequired:
		terminal(platform.Event{Type: "auth_required"}, 0)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-observation.Errors:
			if ok && err != nil {
				emit(events.NewRunErrorEvent("observation unavailable"), 0)
			}
			return
		case e, ok := <-observation.Events:
			if !ok {
				return
			}
			if e.RunID == run && terminal(e, e.Sequence) {
				return
			}
		}
	}
}
func commandError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, platform.ErrConflict), errors.Is(err, platform.ErrBusy):
		status = http.StatusConflict
	case errors.Is(err, platform.ErrForbidden):
		status = http.StatusForbidden
	}
	http.Error(w, http.StatusText(status), status)
}
func lastUserText(messages []types.Message) (string, error) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != types.RoleUser {
			continue
		}
		text, ok := messages[i].Content.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("last user message has no text")
		}
		return text, nil
	}
	return "", fmt.Errorf("user message is required")
}

func targetJourney(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	properties, ok := value.(map[string]any)
	if !ok {
		return "", fmt.Errorf("forwardedProps must be an object")
	}
	target, exists := properties["journeyId"]
	if !exists {
		return "", nil
	}
	name, ok := target.(string)
	if !ok || name == "" {
		return "", fmt.Errorf("journeyId must be a string")
	}
	return name, nil
}
