package chat

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/encoding/sse"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/swiftdiaries/az-agent-platform-go/internal/platform"
)

type handler struct {
	auth     Authenticator
	platform platform.Submitter
	ids      identityMapper
}

func NewHandler(auth Authenticator, service platform.Submitter) http.Handler {
	return &handler{
		auth: auth, platform: service,
		ids: identityMapper{
			threads: make(map[externalThread]string),
			runs:    make(map[externalRun]mappedRun),
			used:    make(map[string]struct{}),
		},
	}
}

func (h *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	principal, err := h.auth.Authenticate(request)
	if err != nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	var input types.RunAgentInput
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20))
	if err := decoder.Decode(&input); err != nil {
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	text, err := lastUserText(input.Messages)
	if err != nil || input.ThreadID == "" || input.RunID == "" {
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	target, err := targetJourney(input.ForwardedProps)
	if err != nil {
		http.Error(writer, "invalid journey target", http.StatusBadRequest)
		return
	}
	internal, err := h.ids.mapIdentity(principal, input.ThreadID, input.RunID)
	if err != nil {
		http.Error(writer, "request failed", http.StatusInternalServerError)
		return
	}
	ctx, span := otel.Tracer("az-agent-platform/chat").Start(request.Context(), "chat.accept")
	span.SetAttributes(attribute.String("thread.id", internal.threadID), attribute.String("run.id", internal.runID))
	defer span.End()
	receipt, err := h.platform.Submit(ctx, platform.Submission{
		Command: platform.Command{
			ThreadID: internal.threadID, RunID: internal.runID, CommunicationID: internal.communicationID,
			Principal: principal, Text: text, TargetJourney: target,
		},
		Headers: request.Header.Clone(),
	})
	if err != nil {
		switch {
		case errors.Is(err, platform.ErrConflict):
			http.Error(writer, "communication conflict", http.StatusConflict)
		case errors.Is(err, platform.ErrForbidden):
			http.Error(writer, "forbidden", http.StatusForbidden)
		case errors.Is(err, platform.ErrAuthRequired):
			writeRunError(request, writer, input.ThreadID, input.RunID, "auth_required", "authentication required")
		default:
			writeRunError(request, writer, input.ThreadID, input.RunID, "internal_error", "request failed")
		}
		return
	}

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	stream := sse.NewSSEWriter()
	messageID := input.RunID + ":answer"
	for _, event := range []events.Event{
		events.NewRunStartedEvent(input.ThreadID, input.RunID),
		events.NewTextMessageStartEvent(messageID, events.WithRole("assistant")),
		events.NewTextMessageContentEvent(messageID, receipt.Answer),
		events.NewTextMessageEndEvent(messageID),
		events.NewRunFinishedEvent(input.ThreadID, input.RunID),
	} {
		if err := stream.WriteEvent(request.Context(), writer, event); err != nil {
			return
		}
	}
}

func writeRunError(request *http.Request, writer http.ResponseWriter, threadID, runID, code, message string) {
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	stream := sse.NewSSEWriter()
	_ = stream.WriteEvent(request.Context(), writer, events.NewRunStartedEvent(threadID, runID))
	_ = stream.WriteEvent(request.Context(), writer, events.NewRunErrorEvent(message, events.WithErrorCode(code), events.WithRunID(runID)))
}

type externalThread struct {
	principal string
	threadID  string
}

type externalRun struct {
	thread externalThread
	runID  string
}

type mappedRun struct {
	threadID        string
	runID           string
	communicationID string
}

type identityMapper struct {
	mu      sync.Mutex
	threads map[externalThread]string
	runs    map[externalRun]mappedRun
	used    map[string]struct{}
}

func (m *identityMapper) mapIdentity(principal, threadID, runID string) (mappedRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	threadKey := externalThread{principal: principal, threadID: threadID}
	internalThreadID := m.threads[threadKey]
	if internalThreadID == "" {
		var err error
		internalThreadID, err = m.newID("thread_")
		if err != nil {
			return mappedRun{}, err
		}
		m.threads[threadKey] = internalThreadID
	}
	runKey := externalRun{thread: threadKey, runID: runID}
	if internal, ok := m.runs[runKey]; ok {
		return internal, nil
	}
	internalRunID, err := m.newID("run_")
	if err != nil {
		return mappedRun{}, err
	}
	communicationID, err := m.newID("communication_")
	if err != nil {
		return mappedRun{}, err
	}
	internal := mappedRun{threadID: internalThreadID, runID: internalRunID, communicationID: communicationID}
	m.runs[runKey] = internal
	return internal, nil
}

func (m *identityMapper) newID(prefix string) (string, error) {
	for {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", fmt.Errorf("generate opaque identity: %w", err)
		}
		candidate := prefix + hex.EncodeToString(random[:])
		if _, exists := m.used[candidate]; exists {
			continue
		}
		m.used[candidate] = struct{}{}
		return candidate, nil
	}
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
