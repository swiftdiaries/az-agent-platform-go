package runtime

import (
	"context"
	"crypto/rand"
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
	"go.opentelemetry.io/otel/trace"

	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	platformmcp "github.com/swiftdiaries/az-agent-platform-go/internal/mcp"
	platformskills "github.com/swiftdiaries/az-agent-platform-go/internal/skills"
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
	Handoff          Handoff
	Context          []string
	History          json.RawMessage
	PendingCommands  []journal.Input
	Instructions     string
	Messages         []string
	Tools            []ModelTool
	ToolResults      []ToolResult
	DefinitionDigest string
	SkillCatalog     []platformskills.Metadata
	SkillMaterial    []platformskills.Material
	Provenance       RequestProvenance
}

type RequestProvenance struct {
	Definition, System, Handoff, Catalog, Context, History MaterialProvenance
	Pending, IncludedInput                                 []MaterialProvenance
}

type MaterialProvenance struct{ ID, Digest string }

type ToolCall struct {
	CallID    string
	Name      string
	Arguments json.RawMessage
}

type ModelResponse struct {
	Text      string
	ToolCalls []ToolCall
	ToolCall  *ToolCall
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
	Store            *journal.Store
	Owner            journal.Owner
	Iteration        int
	History          json.RawMessage
	ThreadID         string
	RunID            string
	Principal        string
	Text             string
	TargetJourney    string
	DefinitionDigest string
	Headers          http.Header
}

type RunOutput struct {
	Waiting   bool
	History   json.RawMessage
	JourneyID string
	Answer    string
	CallID    string
	ToolName  string
}

func NewRunner(registry *definitions.Registry, client *platformmcp.Client, model Model) *Runner {
	return &Runner{definitions: registry, mcp: client, model: model}
}

// Binding resolves an existing session's pin or the database current pointer for a new one.
func (r *Runner) Binding(ctx context.Context, in RunInput) (string, string, error) {
	journey, err := r.route(ctx, in.TargetJourney, in.Text)
	if err != nil {
		return "", "", err
	}
	if in.Store == nil {
		return "", "", journal.ErrDefinition
	}
	digest, err := in.Store.ResolveDefinition(ctx, in.ThreadID, journey.ID, journey.Digest)
	if err != nil {
		return "", "", err
	}
	if _, ok := r.definitions.Version(digest); !ok {
		return "", "", journal.ErrDefinition
	}
	return journey.ID, digest, nil
}

func (r *Runner) Run(ctx context.Context, input RunInput) (RunOutput, error) {
	journey, ok := r.definitions.Version(input.DefinitionDigest)
	if !ok || journey.ID != input.TargetJourney {
		return RunOutput{}, journal.ErrDefinition
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
	return r.runHarness(ctx, input, journey, bound, server)
}

func modelRun(parent context.Context, model Model, callBase string, before func(context.Context, *ModelRequest) error, observed func(context.Context) error) agent.RunFunc {
	return func(ctx context.Context, messages []*message.Message, options ...agent.Option) iter.Seq2[*agent.ResponseUpdate, error] {
		return func(yield func(*agent.ResponseUpdate, error) bool) {
			ctx = trace.ContextWithSpanContext(ctx, trace.SpanContextFromContext(parent))
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
			calls := response.ToolCalls
			if response.ToolCall != nil {
				calls = append(calls, *response.ToolCall)
			}
			if len(calls) > 0 {
				update.FinishReason = "tool_calls"
				for index, call := range calls {
					id := call.CallID
					if id == "" {
						id = fmt.Sprintf("%s-%d-%s", callBase, index, rand.Text())
					}
					update.Contents = append(update.Contents, &message.FunctionCallContent{CallID: id, Name: call.Name, Arguments: string(call.Arguments)})
				}
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

func stableCallID(threadID, runID string) string {
	sum := sha256.Sum256([]byte(threadID + "\x00" + runID + "\x00tool-1"))
	return "call_" + hex.EncodeToString(sum[:8])
}

func digestValue(value any) string {
	data, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func cloneSkillMaterial(in []platformskills.Material) []platformskills.Material {
	out := make([]platformskills.Material, len(in))
	for i, material := range in {
		out[i] = material
		out[i].Body = append([]byte(nil), material.Body...)
	}
	return out
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
