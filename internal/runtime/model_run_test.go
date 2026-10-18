package runtime

import (
	"context"
	"testing"

	"github.com/microsoft/agent-framework-go/message"
)

type captureModel struct{ request ModelRequest }

func (m *captureModel) Complete(_ context.Context, request ModelRequest) (ModelResponse, error) {
	m.request = request
	return ModelResponse{Text: "done"}, nil
}

func TestModelRunNamesToolResultsAfterTheirCall(t *testing.T) {
	model := &captureModel{}
	run := modelRun(context.Background(), model, "base",
		func(context.Context, *ModelRequest) error { return nil },
		func(context.Context) error { return nil })
	history := []*message.Message{
		{Role: message.RoleAssistant, Contents: message.Contents{&message.FunctionCallContent{CallID: "c1", Name: "lookup_alias", Arguments: "{}"}}},
		{Role: message.RoleTool, Contents: message.Contents{&message.FunctionResultContent{CallID: "c1", Result: "ok"}}},
		{Role: message.RoleTool, Contents: message.Contents{&message.FunctionResultContent{CallID: "orphan", Result: "x"}}},
	}
	for _, err := range run(context.Background(), history) {
		if err != nil {
			t.Fatal(err)
		}
	}
	got := model.request.ToolResults
	if len(got) != 2 || got[0].CallID != "c1" || got[0].Name != "lookup_alias" || got[1].CallID != "orphan" || got[1].Name != "" {
		t.Fatalf("tool results = %+v", got)
	}
}
