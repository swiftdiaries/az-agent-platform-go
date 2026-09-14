package runtime

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/tool"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	platformmcp "github.com/swiftdiaries/az-agent-platform-go/internal/mcp"
)

type questionTool struct{}

func (questionTool) Name() string { return "request_user_input" }
func (questionTool) Description() string {
	return "Ask one to three structured clarification questions. An answer never approves a business action."
}
func (questionTool) ReturnSchema() any { return nil }
func (questionTool) Schema() any {
	var schema any
	_ = json.Unmarshal([]byte(`{"type":"object","additionalProperties":false,"required":["questions"],"properties":{"questions":{"type":"array","minItems":1,"maxItems":3,"items":{"type":"object","additionalProperties":false,"required":["id","header","question","options"],"properties":{"id":{"type":"string"},"header":{"type":"string","maxLength":12},"question":{"type":"string"},"options":{"type":"array","minItems":2,"maxItems":3,"items":{"type":"object","additionalProperties":false,"required":["label","description"],"properties":{"label":{"type":"string"},"description":{"type":"string"}}}}}}}}}`), &schema)
	return schema
}

func (r *Runner) runHarness(ctx context.Context, in RunInput, journey definitions.Journey, bound *platformmcp.Bound, server definitions.MCPServer) (output RunOutput, runErr error) {
	var history []*message.Message
	if err := json.Unmarshal(in.History, &history); err != nil {
		return RunOutput{}, err
	}
	// Preserve completed results on a terminal failure and close any unexecuted
	// proposal before a later user turn. This records no claim about remote cancellation.
	defer func() {
		if runErr == nil || len(history) == 0 {
			return
		}
		open := map[string]bool{}
		var order []string
		for _, m := range history {
			for _, c := range m.Contents {
				switch content := c.(type) {
				case *message.FunctionCallContent:
					if !open[content.CallID] {
						order = append(order, content.CallID)
					}
					open[content.CallID] = true
				case *message.FunctionResultContent:
					delete(open, content.CallID)
				}
			}
		}
		for _, id := range order {
			if open[id] {
				history = append(history, resultMessage(id, map[string]string{"error": "run_stopped"}))
				delete(open, id)
			}
		}
		output.History, _ = json.Marshal(history)
	}()
	var pending []journal.Input
	var selected []string
	var requestHistory json.RawMessage
	// The pinned graph resumes each outstanding tool result separately. The product
	// owns batching and durable waiting; a bare MAF agent handles one provider turn.
	tools := append(funcsAsTools(bound.Tools()), tool.Tool(questionTool{}))
	inner := agent.New(agent.ProviderConfig{ProviderName: "configured-model", Run: modelRun(ctx, r.model, stableCallID(in.ThreadID, in.RunID), func(ctx context.Context, req *ModelRequest) error {
		req.Handoff = Handoff{JourneyID: journey.ID, Text: in.Text}
		req.Context = selected
		req.PendingCommands = pending
		req.History = requestHistory
		return in.Store.Check(ctx, in.Owner)
	}, func(ctx context.Context) error { return in.Store.Included(ctx, in.Owner, pending) })}, agent.Config{ID: "journey:" + journey.ID, Name: journey.ID, Description: journey.Description, Tools: tools, RunOptions: []agent.Option{agent.WithInstructions(journey.Prompt)}})
	continuation, err := in.Store.Continuation(ctx, in.Owner)
	if err != nil {
		return RunOutput{}, err
	}
	repairBudget := 1
	var approved *journal.Interaction
	if continuation != nil && in.Iteration == 0 {
		var checkpoint struct {
			Mode    string `json:"mode"`
			Repairs int    `json:"repairs"`
		}
		if journal.DecodeStrict(continuation.Checkpoint, &checkpoint) != nil || checkpoint.Mode != "product_resume_in_place" {
			return RunOutput{}, journal.ErrState
		}
		repairBudget = checkpoint.Repairs
		if err = json.Unmarshal(continuation.History, &history); err != nil {
			return RunOutput{}, err
		}
		i := continuation.Interaction
		if i.Kind == "approval" && continuation.Reply.Decision == "approve" {
			approved = &i
		} else {
			var result any = continuation.Reply
			if i.Kind == "approval" {
				result = map[string]string{"error": "approval_denied"}
			}
			m := resultMessage(i.Call.ProviderCallID, result)
			history = append(history, m)
		}
	}
	invoke := func() (*agent.Response, error) {
		inbox, err := in.Store.Pending(ctx, in.Owner)
		if err != nil {
			return nil, err
		}
		pending = inbox.Commands
		selected = inbox.Context
		requestHistory, err = json.Marshal(history)
		if err != nil {
			return nil, err
		}
		beforeInput := len(history)
		for _, c := range pending {
			if c.Text != "" {
				m := &message.Message{Role: message.RoleUser, Contents: message.Contents{&message.TextContent{Text: c.Text}}}
				history = append(history, m)
			}
		}
		// Function results precede newly admitted user messages in materialized history.
		response, err := inner.Run(ctx, history).Collect()
		if err == nil {
			history = append(history, response.Messages...)
		} else {
			history = history[:beforeInput]
		}
		return response, err
	}
	var calls []*message.FunctionCallContent
	if approved != nil {
		calls = []*message.FunctionCallContent{{CallID: approved.Call.ProviderCallID, Name: approved.Call.Name, Arguments: string(approved.Call.Arguments)}}
	}
	callNumber := 0
	for round := 0; round < 16; round++ {
		if len(calls) == 0 {
			response, err := invoke()
			if err != nil {
				return RunOutput{}, fmt.Errorf("model request failed: %w", err)
			}
			for c := range response.Contents() {
				if call, ok := c.(*message.FunctionCallContent); ok && !call.InformationalOnly {
					calls = append(calls, call)
				}
			}
			if len(calls) == 0 {
				if response.String() == "" {
					return RunOutput{}, fmt.Errorf("model returned no answer")
				}
				serialized, err := json.Marshal(history)
				return RunOutput{JourneyID: journey.ID, Answer: response.String(), History: serialized}, err
			}
		}
		for index, call := range calls {
			callNumber++
			binding, err := journal.ActionBinding(call.Name, []byte(call.Arguments))
			if err != nil {
				return RunOutput{}, err
			}
			callID := stableCallID(in.ThreadID, fmt.Sprintf("%s/%d/%d", in.RunID, in.Iteration, callNumber))
			policy := server.Policies[call.Name].Class
			if policy == "" {
				policy = "effectful"
			}
			if call.Name != "request_user_input" {
				allowed := false
				for _, name := range journey.MCP.Tools {
					allowed = allowed || name == call.Name
				}
				if !allowed {
					return RunOutput{}, fmt.Errorf("tool is not allowed")
				}
			}
			approvalID := ""
			if approved != nil && approved.Call.ProviderCallID == call.CallID {
				if approved.Call.Binding != binding {
					return RunOutput{}, journal.ErrState
				}
				callID = approved.Call.CallID
				approvalID = approved.ID
			}
			if call.Name == "request_user_input" || (policy != "read_only" && approvalID == "") {
				// Close siblings explicitly before checkpointing one externally answerable
				// request; they may be proposed again after this wait, never auto-dispatched.
				for _, sibling := range calls[index+1:] {
					m := resultMessage(sibling.CallID, map[string]string{"error": "deferred_for_human_input"})
					history = append(history, m)
				}

				i := journal.Interaction{ID: "interaction_" + rand.Text(), Kind: "approval", Call: journal.ProposedCall{CallID: callID, ProviderCallID: call.CallID, Name: call.Name, Arguments: json.RawMessage(call.Arguments), Binding: binding}}
				if call.Name == "request_user_input" {
					i.Kind = "clarification"
					var args struct {
						Questions []journal.Question `json:"questions"`
					}
					if err := journal.DecodeStrict([]byte(call.Arguments), &args); err != nil {
						return RunOutput{}, err
					}
					i.Questions = args.Questions
				}
				checkpoint, err := json.Marshal(struct {
					Mode    string `json:"mode"`
					Repairs int    `json:"repairs"`
				}{"product_resume_in_place", repairBudget})
				if err != nil {
					return RunOutput{}, err
				}
				serialized, err := json.Marshal(history)
				if err != nil {
					return RunOutput{}, err
				}
				err = in.Store.Wait(ctx, in.Owner, i, checkpoint, serialized)
				if errors.Is(err, journal.ErrPending) {
					m := resultMessage(call.CallID, map[string]string{"error": "superseded_by_steering"})
					history = append(history, m)
					break
				}
				if err != nil {
					return RunOutput{}, err
				}
				return RunOutput{JourneyID: journey.ID, History: serialized, Waiting: true}, nil
			}
			result, err := guardedCall(ctx, in, bound, journal.Operation{CallID: callID, Name: call.Name, Arguments: call.Arguments, Binding: binding, Policy: policy}, approvalID)
			if err != nil {
				return RunOutput{}, err
			}
			if failure, ok := result.(map[string]any); ok && failure["error"] == "business_rejected" {
				if repairBudget == 0 {
					return RunOutput{}, fmt.Errorf("business repair budget exhausted")
				}
				repairBudget--
			}
			m := resultMessage(call.CallID, result)
			history = append(history, m)
			approved = nil
		}
		calls = nil
	}
	return RunOutput{}, fmt.Errorf("provider tool round budget exhausted")
}
func resultMessage(id string, result any) *message.Message {
	return &message.Message{Role: message.RoleTool, Contents: message.Contents{&message.FunctionResultContent{CallID: id, Result: result}}}
}
