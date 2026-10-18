package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"

	"github.com/swiftdiaries/az-agent-platform-go/internal/cost"
	"github.com/swiftdiaries/az-agent-platform-go/internal/settings"
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
		func(context.Context) error { return nil },
		cost.Access{}, func(context.Context, cost.Tokens) error { return nil })
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

type usageModel struct {
	usage cost.Tokens
	err   error
}

func (m usageModel) Complete(context.Context, ModelRequest) (ModelResponse, error) {
	return ModelResponse{Text: "done", Usage: m.usage}, m.err
}

func drain(t *testing.T, model Model, usage func(context.Context, cost.Tokens) error) error {
	t.Helper()
	run := modelRun(context.Background(), model, "base",
		func(context.Context, *ModelRequest) error { return nil },
		func(context.Context) error { return nil }, cost.Access{}, usage)
	var first error
	for _, err := range run(context.Background(), nil) {
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}

func TestModelRunRecordsUsageOncePerSuccessfulCompletion(t *testing.T) {
	var got []cost.Tokens
	want := cost.Tokens{Model: "m", Input: 3, Output: 4, Cached: 1}
	if err := drain(t, usageModel{usage: want}, func(_ context.Context, u cost.Tokens) error { got = append(got, u); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("recorded = %+v, want [%+v]", got, want)
	}
	got = nil
	if err := drain(t, usageModel{err: errors.New("boom")}, func(_ context.Context, u cost.Tokens) error { got = append(got, u); return nil }); err == nil {
		t.Fatal("model error swallowed")
	}
	if len(got) != 0 {
		t.Fatalf("usage recorded for a failed call: %+v", got)
	}
}

func TestModelRunUsageRecordErrorFailsRound(t *testing.T) {
	sentinel := errors.New("record failed")
	if err := drain(t, usageModel{}, func(context.Context, cost.Tokens) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want record error", err)
	}
}

func TestModelRunLimitErrorSurvivesFrameworkWrapping(t *testing.T) {
	limit := &cost.LimitError{Kind: cost.LimitRate, RetryAfter: 5 * time.Second}
	run := modelRun(context.Background(), usageModel{err: limit}, "base",
		func(context.Context, *ModelRequest) error { return nil },
		func(context.Context) error { return nil }, cost.Access{}, func(context.Context, cost.Tokens) error { return nil })
	a := agent.New(agent.ProviderConfig{ProviderName: "limit", Run: run}, agent.Config{ID: "x", Name: "x"})
	_, err := a.Run(context.Background(), nil).Collect()
	var got *cost.LimitError
	if !errors.As(err, &got) || got.Kind != cost.LimitRate || got.RetryAfter != 5*time.Second {
		t.Fatalf("err = %v, want *cost.LimitError through the agent", err)
	}
}

type gateStub struct {
	decision cost.Decision
	admitErr error
	access   cost.Access
	accErr   error
	seen     []cost.Attribution
}

func (g *gateStub) Admit(_ context.Context, a cost.Attribution, _ settings.Cost) (cost.Decision, error) {
	g.seen = append(g.seen, a)
	return g.decision, g.admitErr
}

func (g *gateStub) Access(_ context.Context, a cost.Attribution, _ settings.Cost) (cost.Access, error) {
	return g.access, g.accErr
}

func TestRunnerAdmit(t *testing.T) {
	in := RunInput{Tenant: "tenant-secret-a", Principal: "principal-secret-b", ThreadID: "thread-c", RunID: "run-d", TargetJourney: "journey-e"}
	t.Run("nil gate admits", func(t *testing.T) {
		a, _, err := NewRunner(nil, nil, Tenancy{}).Admit(context.Background(), in)
		if err != nil || a != (cost.Attribution{Tenant: "tenant-secret-a", Principal: "principal-secret-b", Conversation: "thread-c", Run: "run-d", Journey: "journey-e"}) {
			t.Fatalf("attribution = %+v, err = %v", a, err)
		}
	})
	t.Run("denial is the limit error", func(t *testing.T) {
		g := &gateStub{decision: cost.Deny(cost.LimitBudget, 7*time.Second)}
		_, _, err := NewRunner(nil, nil, Tenancy{Cost: g}).Admit(context.Background(), in)
		var le *cost.LimitError
		if !errors.As(err, &le) || le.Kind != cost.LimitBudget || le.RetryAfter != 7*time.Second {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("gate errors are dependency_unavailable without attribution", func(t *testing.T) {
		for _, g := range []*gateStub{{decision: cost.Allow(), admitErr: errors.New("down")}, {decision: cost.Allow(), accErr: errors.New("down")}} {
			_, _, err := NewRunner(nil, nil, Tenancy{Cost: g}).Admit(context.Background(), in)
			if !errors.Is(err, ErrDependencyUnavailable) {
				t.Fatalf("err = %v", err)
			}
			for _, v := range []string{"tenant-secret-a", "principal-secret-b", "thread-c", "run-d", "journey-e"} {
				if strings.Contains(err.Error(), v) {
					t.Fatalf("error echoes attribution %q: %v", v, err)
				}
			}
		}
	})
}
