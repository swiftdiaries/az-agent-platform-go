package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

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
}

func NewHandler(auth Authenticator, service platform.Submitter) http.Handler {
	return &handler{auth: auth, platform: service}
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
	ctx, span := otel.Tracer("az-agent-platform/chat").Start(request.Context(), "chat.accept")
	span.SetAttributes(attribute.String("thread.id", input.ThreadID), attribute.String("run.id", input.RunID))
	defer span.End()
	receipt, err := h.platform.Submit(ctx, platform.Submission{
		Command: platform.Command{
			ThreadID: input.ThreadID, RunID: input.RunID, CommunicationID: input.RunID,
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
			writeRunError(request, writer, receipt, "auth_required", "authentication required")
		default:
			writeRunError(request, writer, receipt, "internal_error", "request failed")
		}
		return
	}

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	stream := sse.NewSSEWriter()
	messageID := receipt.RunID + ":answer"
	for _, event := range []events.Event{
		events.NewRunStartedEvent(receipt.ThreadID, receipt.RunID),
		events.NewTextMessageStartEvent(messageID, events.WithRole("assistant")),
		events.NewTextMessageContentEvent(messageID, receipt.Answer),
		events.NewTextMessageEndEvent(messageID),
		events.NewRunFinishedEvent(receipt.ThreadID, receipt.RunID),
	} {
		if err := stream.WriteEvent(request.Context(), writer, event); err != nil {
			return
		}
	}
}

func writeRunError(request *http.Request, writer http.ResponseWriter, receipt platform.Receipt, code, message string) {
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	stream := sse.NewSSEWriter()
	_ = stream.WriteEvent(request.Context(), writer, events.NewRunStartedEvent(receipt.ThreadID, receipt.RunID))
	_ = stream.WriteEvent(request.Context(), writer, events.NewRunErrorEvent(message, events.WithErrorCode(code), events.WithRunID(receipt.RunID)))
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
