package runtime

import (
	"context"
	"encoding/json"
	"iter"
	"testing"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/workflow"
	"github.com/microsoft/agent-framework-go/workflow/agentworkflow"
)

// This is a contract against the pinned dependency, not a product resume simulation.
func TestHITLFrameworkGraphContinuationContract(t *testing.T) {
	var observed []*message.Message
	build := func() *agent.Agent {
		inner := agent.New(agent.ProviderConfig{ProviderName: "contract", Run: func(_ context.Context, msgs []*message.Message, _ ...agent.Option) iter.Seq2[*agent.ResponseUpdate, error] {
			return func(yield func(*agent.ResponseUpdate, error) bool) {
				observed = msgs
				for _, m := range msgs {
					for _, c := range m.Contents {
						if result, ok := c.(*message.FunctionResultContent); ok {
							if m.Role != message.RoleTool || result.CallID != "question-call" {
								t.Errorf("result lost role/correlation: %s %s", m.Role, result.CallID)
							}
							yield(&agent.ResponseUpdate{Role: message.RoleAssistant, Contents: message.Contents{&message.TextContent{Text: "answered"}}}, nil)
							return
						}
					}
				}
				yield(&agent.ResponseUpdate{Role: message.RoleAssistant, Contents: message.Contents{&message.FunctionCallContent{CallID: "question-call", Name: "request_user_input", Arguments: `{"question":"choose"}`}}}, nil)
			}
		}}, agent.Config{ID: "contract-agent", Name: "contract-agent"})
		host := agentworkflow.New(inner, agentworkflow.Config{EmitUpdateEvents: new(true)})
		graph, err := workflow.NewBuilder(host).WithOutputFrom(host).Build()
		if err != nil {
			t.Fatal(err)
		}
		outer, err := agentworkflow.NewAgent(graph, agentworkflow.AgentConfig{})
		if err != nil {
			t.Fatal(err)
		}
		return outer
	}
	first := build()
	session, err := first.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	response, err := first.RunText(t.Context(), "start", agent.WithSession(session)).Collect()
	if err != nil {
		t.Fatal(err)
	}
	var call *message.FunctionCallContent
	for c := range response.Contents() {
		if fc, ok := c.(*message.FunctionCallContent); ok {
			call = fc
		}
	}
	if call == nil {
		t.Fatal("graph did not wait")
	}
	checkpoint, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	var before struct {
		States map[string]struct {
			Pending map[string]json.RawMessage `json:"pending"`
		} `json:"State"`
	}
	if err := json.Unmarshal(checkpoint, &before); err != nil {
		t.Fatal(err)
	}
	if len(before.States["workflowprovider_state"].Pending) != 1 {
		t.Fatalf("not a persisted graph wait: %s", checkpoint)
	}
	var restored agent.Session
	if err = json.Unmarshal(checkpoint, &restored); err != nil {
		t.Fatal(err)
	}
	second := build()
	final, err := second.RunMessage(t.Context(), &message.Message{Role: message.RoleTool, Contents: message.Contents{&message.FunctionResultContent{CallID: call.CallID, Result: map[string]string{"answer": "yes"}}}}, agent.WithSession(&restored)).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if final.String() != "answered" {
		t.Fatalf("resume output %q", final.String())
	}
	end, err := json.Marshal(&restored)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(end, &before); err != nil {
		t.Fatal(err)
	}
	var after struct {
		States map[string]struct {
			Pending map[string]json.RawMessage `json:"pending"`
		} `json:"State"`
	}
	if err = json.Unmarshal(end, &after); err != nil {
		t.Fatal(err)
	}
	if len(after.States["workflowprovider_state"].Pending) != 0 {
		t.Fatalf("graph still waiting: %s", end)
	}
	found := false
	for _, m := range observed {
		for _, c := range m.Contents {
			if _, ok := c.(*message.FunctionResultContent); ok {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("materialized history did not change")
	}
}

// The negative contract records why production uses the product-owned continuation.
func TestHITLFrameworkMultipleContinuationLimitation(t *testing.T) {
	var observed []*message.Message
	invocations := 0
	partial := false
	build := func() *agent.Agent {
		inner := agent.New(agent.ProviderConfig{ProviderName: "contract", Run: func(_ context.Context, msgs []*message.Message, _ ...agent.Option) iter.Seq2[*agent.ResponseUpdate, error] {
			return func(yield func(*agent.ResponseUpdate, error) bool) {
				observed = msgs
				invocations++
				results := 0
				for _, m := range msgs {
					for _, c := range m.Contents {
						if _, ok := c.(*message.FunctionResultContent); ok {
							results++
						}
					}
				}
				if results != 0 && results != 2 {
					partial = true
				}
				for _, m := range msgs {
					for _, c := range m.Contents {
						if result, ok := c.(*message.FunctionResultContent); ok {
							if m.Role != message.RoleTool || (result.CallID != "question-call" && result.CallID != "sibling-call") {
								t.Errorf("result lost role/correlation: %s %s", m.Role, result.CallID)
							}
							yield(&agent.ResponseUpdate{Role: message.RoleAssistant, Contents: message.Contents{&message.TextContent{Text: "answered"}}}, nil)
							return
						}
					}
				}
				yield(&agent.ResponseUpdate{Role: message.RoleAssistant, Contents: message.Contents{&message.FunctionCallContent{CallID: "question-call", Name: "request_user_input", Arguments: `{"question":"choose"}`}, &message.FunctionCallContent{CallID: "sibling-call", Name: "lookup", Arguments: `{}`}}}, nil)
			}
		}}, agent.Config{ID: "contract-agent", Name: "contract-agent"})
		host := agentworkflow.New(inner, agentworkflow.Config{EmitUpdateEvents: new(true)})
		graph, err := workflow.NewBuilder(host).WithOutputFrom(host).Build()
		if err != nil {
			t.Fatal(err)
		}
		outer, err := agentworkflow.NewAgent(graph, agentworkflow.AgentConfig{})
		if err != nil {
			t.Fatal(err)
		}
		return outer
	}
	first := build()
	session, err := first.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	response, err := first.RunText(t.Context(), "start", agent.WithSession(session)).Collect()
	if err != nil {
		t.Fatal(err)
	}
	var call *message.FunctionCallContent
	for c := range response.Contents() {
		if fc, ok := c.(*message.FunctionCallContent); ok {
			call = fc
		}
	}
	if call == nil {
		t.Fatal("graph did not wait")
	}
	checkpoint, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	var before struct {
		States map[string]struct {
			Pending map[string]json.RawMessage `json:"pending"`
		} `json:"State"`
	}
	if err := json.Unmarshal(checkpoint, &before); err != nil {
		t.Fatal(err)
	}
	if len(before.States["workflowprovider_state"].Pending) != 2 {
		t.Fatalf("not a persisted graph wait: %s", checkpoint)
	}
	var restored agent.Session
	if err = json.Unmarshal(checkpoint, &restored); err != nil {
		t.Fatal(err)
	}
	second := build()
	var replies []*message.Message
	for id := range before.States["workflowprovider_state"].Pending {
		replies = append(replies, &message.Message{Role: message.RoleTool, Contents: message.Contents{&message.FunctionResultContent{CallID: id, Result: "answered"}}})
	}
	final, err := second.Run(t.Context(), replies, agent.WithSession(&restored)).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if final.String() != "answered\nanswered" {
		t.Fatalf("resume output %q", final.String())
	}
	end, err := json.Marshal(&restored)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(end, &before); err != nil {
		t.Fatal(err)
	}
	var after struct {
		States map[string]struct {
			Pending map[string]json.RawMessage `json:"pending"`
		} `json:"State"`
	}
	if err = json.Unmarshal(end, &after); err != nil {
		t.Fatal(err)
	}
	if len(after.States["workflowprovider_state"].Pending) != 0 {
		t.Fatalf("graph still waiting: %s", end)
	}
	found := false
	for _, m := range observed {
		for _, c := range m.Contents {
			if _, ok := c.(*message.FunctionResultContent); ok {
				found = true
			}
		}
	}
	if invocations != 3 || !partial {
		t.Fatalf("framework limitation changed: invocations=%d partial=%v; rerun continuation qualification", invocations, partial)
	}
	if !found {
		t.Fatal("materialized history did not change")
	}
}
