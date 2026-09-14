package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/tool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	platformmcp "github.com/swiftdiaries/az-agent-platform-go/internal/mcp"
)

type ModelTool struct {
	Name        string
	Description string
	Schema      any
}

type ToolResult struct {
	CallID string
	Name   string
	Value  any
}

type Handoff struct{ JourneyID, Text string }

type ModelRequest struct {
	Handoff         Handoff
	Context         []string
	History         json.RawMessage
	PendingCommands []journal.Input
	Instructions    string
	Messages        []string
	Tools           []ModelTool
	ToolResults     []ToolResult
}

type ToolCall struct {
	CallID    string
	Name      string
	Arguments json.RawMessage
}

type ModelResponse struct {
	Text     string
	ToolCall *ToolCall
}

type Model interface {
	Complete(context.Context, ModelRequest) (ModelResponse, error)
}

type Runner struct {
	definitions *definitions.Registry
	mcp         *platformmcp.Client
	model       Model
}

type RunInput struct {
	Store         *journal.Store
	Owner         journal.Owner
	Iteration     int
	History       json.RawMessage
	ThreadID      string
	RunID         string
	Principal     string
	Text          string
	TargetJourney string
	Headers       http.Header
}

type RunOutput struct {
	History   json.RawMessage
	JourneyID string
	Answer    string
	CallID    string
	ToolName  string
}

func NewRunner(registry *definitions.Registry, client *platformmcp.Client, model Model) *Runner {
	return &Runner{definitions: registry, mcp: client, model: model}
}

// Binding identifies exactly the loaded journey declaration and prompt used by this runner.
func (r *Runner) Binding(ctx context.Context, in RunInput) (string, string, error) {
	journey, err := r.route(ctx, in.TargetJourney, in.Text)
	if err != nil {
		return "", "", err
	}
	data, err := json.Marshal(struct {
		Definition definitions.Journey
		Prompt     string
	}{journey, journey.Prompt})
	if err != nil {
		return "", "", err
	}
	return journey.ID, fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func (r *Runner) Run(ctx context.Context, input RunInput) (RunOutput, error) {
	journey, err := r.route(ctx, input.TargetJourney, input.Text)
	if err != nil {
		return RunOutput{}, err
	}
	ctx, span := otel.Tracer("az-agent-platform/runtime").Start(ctx, "journey.run")
	span.SetAttributes(
		attribute.String("thread.id", input.ThreadID), attribute.String("run.id", input.RunID),
		attribute.String("journey.id", journey.ID),
	)
	defer span.End()
	server, ok := r.definitions.Server(journey.MCP.Server)
	if !ok {
		return RunOutput{}, fmt.Errorf("journey MCP server is unavailable")
	}
	if err := input.Store.Check(ctx, input.Owner); err != nil {
		return RunOutput{}, err
	}
	bound, err := r.mcp.Bind(ctx, server, journey.MCP.Tools, input.Headers)
	if err != nil {
		return RunOutput{}, err
	}
	defer bound.Close()

	if err := input.Store.Check(ctx, input.Owner); err != nil {
		return RunOutput{}, err
	}
	productCallID := stableCallID(input.ThreadID, fmt.Sprintf("%s/%d", input.RunID, input.Iteration))
	var pending []journal.Input
	var selected []string
	var requestHistory json.RawMessage
	mafAgent := agent.New(agent.ProviderConfig{
		ProviderName: "configured-model",
		Run: modelRun(r.model, productCallID, func(ctx context.Context, request *ModelRequest) error {
			request.Handoff = Handoff{JourneyID: journey.ID, Text: input.Text}
			request.Context = selected
			request.History = requestHistory
			request.PendingCommands = pending
			return input.Store.Check(ctx, input.Owner)
		}, func(ctx context.Context) error { return input.Store.Included(ctx, input.Owner, pending) }),
	}, agent.Config{
		ID:          "journey:" + journey.ID,
		Name:        journey.ID,
		Description: journey.Description,
		Tools:       funcsAsTools(bound.Tools()),
		RunOptions:  []agent.Option{agent.WithInstructions(journey.Prompt)},
	})
	var history []*message.Message
	if len(input.History) > 0 {
		if err := json.Unmarshal(input.History, &history); err != nil {
			return RunOutput{}, fmt.Errorf("decode provider history: %w", err)
		}
	}
	// MAF sessions remain reconstructable: pass the complete authoritative history
	// to each invocation, including the inbox drained at this provider boundary.
	invoke := func() (*agent.Response, error) {
		inbox, err := input.Store.Pending(ctx, input.Owner)
		if err != nil {
			return nil, err
		}
		pending = inbox.Commands
		selected = inbox.Context
		requestHistory, err = json.Marshal(history)
		if err != nil {
			return nil, err
		}
		for _, command := range pending {
			history = append(history, &message.Message{Role: message.RoleUser, Contents: message.Contents{&message.TextContent{Text: command.Text}}})
		}
		return mafAgent.Run(ctx, history).Collect()
	}
	response, err := invoke()
	if err != nil {
		return RunOutput{}, fmt.Errorf("model request failed: %w", err)
	}
	history = append(history, response.Messages...)
	call := firstToolCall(response)
	if call == nil {
		if answer := response.String(); answer != "" {
			serialized, err := json.Marshal(history)
			return RunOutput{JourneyID: journey.ID, Answer: answer, History: serialized}, err
		}
		return RunOutput{}, fmt.Errorf("model returned no answer")
	}
	if !json.Valid([]byte(call.Arguments)) {
		return RunOutput{}, fmt.Errorf("model returned invalid tool arguments")
	}
	if err := input.Store.Check(ctx, input.Owner); err != nil {
		return RunOutput{}, err
	}
	result, err := bound.Call(ctx, productCallID, call.Name, []byte(call.Arguments))
	if checkErr := input.Store.Check(ctx, input.Owner); checkErr != nil {
		return RunOutput{}, checkErr
	}
	if err != nil {
		return RunOutput{}, err
	}
	toolMessage := &message.Message{
		Role: message.RoleTool,
		Contents: message.Contents{&message.FunctionResultContent{
			CallID: call.CallID, Result: result,
		}},
	}
	history = append(history, toolMessage)
	final, err := invoke()
	if err != nil {
		return RunOutput{}, fmt.Errorf("model request failed: %w", err)
	}
	if firstToolCall(final) != nil {
		return RunOutput{}, fmt.Errorf("multiple tool rounds are outside the Task 1 read-only slice")
	}
	if final.String() == "" {
		return RunOutput{}, fmt.Errorf("model returned no answer")
	}
	history = append(history, final.Messages...)
	serialized, err := json.Marshal(history)
	return RunOutput{JourneyID: journey.ID, Answer: final.String(), CallID: productCallID, ToolName: call.Name, History: serialized}, err
}

func modelRun(model Model, callBase string, before func(context.Context, *ModelRequest) error, observed func(context.Context) error) agent.RunFunc {
	return func(ctx context.Context, messages []*message.Message, options ...agent.Option) iter.Seq2[*agent.ResponseUpdate, error] {
		return func(yield func(*agent.ResponseUpdate, error) bool) {
			request := ModelRequest{}
			for instructions := range agent.AllOptions(options, agent.WithInstructions) {
				request.Instructions = instructions
			}
			for candidate := range agent.AllOptions(options, agent.WithTool) {
				schema, _ := candidate.(tool.SchemaTool)
				request.Tools = append(request.Tools, ModelTool{Name: candidate.Name(), Description: candidate.Description(), Schema: schemaValue(schema)})
			}
			for _, msg := range messages {
				if text := msg.String(); text != "" {
					request.Messages = append(request.Messages, text)
				}
				for _, content := range msg.Contents {
					if result, ok := content.(*message.FunctionResultContent); ok {
						request.ToolResults = append(request.ToolResults, ToolResult{CallID: result.CallID, Value: result.Result})
					}
				}
			}
			if err := before(ctx, &request); err != nil {
				yield(nil, err)
				return
			}
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
			modelCtx, span := otel.Tracer("az-agent-platform/runtime").Start(ctx, "model.call")
			span.SetAttributes(attribute.String("call.id", callBase))
			response, err := model.Complete(modelCtx, request)
			span.End()
			if observedErr := observed(ctx); observedErr != nil {
				yield(nil, observedErr)
				return
			}
			if err != nil {
				yield(nil, err)
				return
			}
			update := &agent.ResponseUpdate{Role: message.RoleAssistant}
			if response.ToolCall != nil {
				callID := response.ToolCall.CallID
				if callID == "" {
					callID = callBase
				}
				update.FinishReason = "tool_calls"
				update.Contents = message.Contents{&message.FunctionCallContent{
					CallID: callID, Name: response.ToolCall.Name, Arguments: string(response.ToolCall.Arguments),
				}}
			} else {
				update.FinishReason = "stop"
				update.Contents = message.Contents{&message.TextContent{Text: response.Text}}
			}
			yield(update, nil)
		}
	}
}

func schemaValue(schema tool.SchemaTool) any {
	if schema == nil {
		return nil
	}
	return schema.Schema()
}

func funcsAsTools(functions []tool.FuncTool) []tool.Tool {
	tools := make([]tool.Tool, len(functions))
	for i := range functions {
		tools[i] = functions[i]
	}
	return tools
}

func firstToolCall(response *agent.Response) *message.FunctionCallContent {
	if response == nil {
		return nil
	}
	for content := range response.Contents() {
		if call, ok := content.(*message.FunctionCallContent); ok && !call.InformationalOnly {
			return call
		}
	}
	return nil
}

func stableCallID(threadID, runID string) string {
	sum := sha256.Sum256([]byte(threadID + "\x00" + runID + "\x00tool-1"))
	return "call_" + hex.EncodeToString(sum[:8])
}

// TransientHeaders copies only names approved for the selected MCP server.
func (r *Runner) TransientHeaders(ctx context.Context, target, text string, inbound http.Header) http.Header {
	result := make(http.Header)
	journey, err := r.route(ctx, target, text)
	if err != nil {
		return result
	}
	server, ok := r.definitions.Server(journey.MCP.Server)
	if !ok {
		return result
	}
	for _, name := range server.ForwardHeaders {
		for _, value := range inbound.Values(name) {
			result.Add(name, value)
		}
	}
	return result
}
