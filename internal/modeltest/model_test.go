package modeltest_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"

	"github.com/swiftdiaries/az-agent-platform-go/internal/modeltest"
)

func TestModelStepsByIndex(t *testing.T) {
	boom := errors.New("boom")
	m := modeltest.New(modeltest.Text("hello"), modeltest.Call("lookup", nil), modeltest.Fail(boom))
	ctx := context.Background()

	r0, err := m.Complete(ctx, agentruntime.ModelRequest{})
	if err != nil || r0.Text != "hello" || len(r0.ToolCalls) != 0 {
		t.Fatalf("step 0 = %+v, %v", r0, err)
	}
	r1, err := m.Complete(ctx, agentruntime.ModelRequest{})
	if err != nil || len(r1.ToolCalls) != 1 {
		t.Fatalf("step 1 = %+v, %v", r1, err)
	}
	tc := r1.ToolCalls[0]
	if tc.CallID != "modeltest-call-1" || tc.Name != "lookup" || string(tc.Arguments) != "{}" {
		t.Fatalf("tool call = %+v", tc)
	}
	if r1.ToolCall != nil {
		t.Fatalf("deprecated ToolCall set: %+v", r1.ToolCall)
	}
	if _, err := m.Complete(ctx, agentruntime.ModelRequest{}); !errors.Is(err, boom) {
		t.Fatalf("step 2 err = %v", err)
	}
	if m.Count() != 3 {
		t.Fatalf("Count = %d", m.Count())
	}
}

func TestModelCallArguments(t *testing.T) {
	type payload struct {
		Q string `json:"q"`
		N int    `json:"n"`
	}
	m := modeltest.New(
		modeltest.Call("a", payload{Q: "x", N: 2}),
		modeltest.Call("b", map[string]any{"k": "v"}),
		modeltest.Call("c", nil),
		modeltest.Call("d", json.RawMessage(`{"raw": true}`)),
		modeltest.Call("e", []byte(`{"bytes":1}`)),
		modeltest.Call("f", `{"str":1}`),
		modeltest.Call("g", json.RawMessage(`{bad`)),
	)
	want := []string{`{"q":"x","n":2}`, `{"k":"v"}`, `{}`, `{"raw": true}`, `{"bytes":1}`, `{"str":1}`}
	for i, w := range want {
		resp, err := m.Complete(context.Background(), agentruntime.ModelRequest{})
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got := string(resp.ToolCalls[0].Arguments); got != w {
			t.Fatalf("step %d arguments = %s, want %s", i, got, w)
		}
	}
	if _, err := m.Complete(context.Background(), agentruntime.ModelRequest{}); err == nil {
		t.Fatal("invalid JSON arguments must return an error")
	}
}

func TestModelRecordsRequests(t *testing.T) {
	m := modeltest.New(modeltest.Text("a"), modeltest.Text("b"))
	req := agentruntime.ModelRequest{
		Instructions: "be brief",
		Messages:     []string{"hi"},
		Context:      []string{"ctx"},
		OnDispatch:   func(context.Context) error { return nil },
	}
	if _, err := m.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Complete(context.Background(), agentruntime.ModelRequest{Instructions: "second"}); err != nil {
		t.Fatal(err)
	}
	got := m.Requests()
	if len(got) != 2 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].Instructions != "be brief" || len(got[0].Messages) != 1 || got[0].Messages[0] != "hi" || got[0].Context[0] != "ctx" {
		t.Fatalf("request 0 = %+v", got[0])
	}
	if got[0].OnDispatch != nil {
		t.Fatal("OnDispatch must be nil in the recorded copy")
	}
	if got[1].Instructions != "second" {
		t.Fatalf("request 1 = %+v", got[1])
	}
	// The returned slice is a copy.
	got[0].Instructions = "mutated"
	if m.Requests()[0].Instructions != "be brief" {
		t.Fatal("Requests must return a copy")
	}
}

func TestModelCallsOnDispatch(t *testing.T) {
	m := modeltest.New(modeltest.Text("a"), modeltest.Text("b"))
	calls := 0
	req := agentruntime.ModelRequest{OnDispatch: func(context.Context) error { calls++; return nil }}
	if _, err := m.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("OnDispatch calls = %d, want 1", calls)
	}

	dispatchErr := errors.New("dispatch refused")
	_, err := m.Complete(context.Background(), agentruntime.ModelRequest{
		OnDispatch: func(context.Context) error { return dispatchErr },
	})
	if !errors.Is(err, dispatchErr) {
		t.Fatalf("err = %v, want dispatch error", err)
	}
	if m.Count() != 2 {
		t.Fatalf("Count = %d", m.Count())
	}
}

func TestModelExhausted(t *testing.T) {
	m := modeltest.New(modeltest.Text("only"))
	if _, err := m.Complete(context.Background(), agentruntime.ModelRequest{}); err != nil {
		t.Fatal(err)
	}
	_, err := m.Complete(context.Background(), agentruntime.ModelRequest{Instructions: "extra"})
	if err == nil || !strings.Contains(err.Error(), "modeltest: no step for request 1") {
		t.Fatalf("err = %v", err)
	}
	reqs := m.Requests()
	if len(reqs) != 2 || reqs[1].Instructions != "extra" {
		t.Fatalf("requests = %+v", reqs)
	}
	calls := m.Calls()
	if len(calls) != 2 || calls[1].Err == nil {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestModelWithUsage(t *testing.T) {
	u := modeltest.Usage{InputTokens: 10, OutputTokens: 5, CachedTokens: 3}
	m := modeltest.New(modeltest.Text("a").WithUsage(u), modeltest.Text("b"))
	for range 2 {
		if _, err := m.Complete(context.Background(), agentruntime.ModelRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	calls := m.Calls()
	if calls[0].Usage != u {
		t.Fatalf("usage 0 = %+v", calls[0].Usage)
	}
	if calls[1].Usage != (modeltest.Usage{}) {
		t.Fatalf("usage 1 = %+v", calls[1].Usage)
	}
	if calls[0].Response.Text != "a" || calls[1].Response.Text != "b" {
		t.Fatalf("responses = %+v", calls)
	}
}

func TestModelThen(t *testing.T) {
	m := modeltest.New(modeltest.Text("one"))
	if got := m.Then(modeltest.Text("two")).Then(modeltest.Text("three")); got != m {
		t.Fatal("Then must return the same model")
	}
	for _, want := range []string{"one", "two", "three"} {
		resp, err := m.Complete(context.Background(), agentruntime.ModelRequest{})
		if err != nil || resp.Text != want {
			t.Fatalf("got %+v, %v; want %q", resp, err, want)
		}
	}
}

func TestModelFunc(t *testing.T) {
	m := modeltest.New(modeltest.Func(func(_ context.Context, req agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		return agentruntime.ModelResponse{Text: "echo:" + req.Instructions}, nil
	}))
	resp, err := m.Complete(context.Background(), agentruntime.ModelRequest{Instructions: "x"})
	if err != nil || resp.Text != "echo:x" {
		t.Fatalf("got %+v, %v", resp, err)
	}
}

func TestModelConcurrent(t *testing.T) {
	const n = 50
	steps := make([]modeltest.Step, n)
	for i := range steps {
		steps[i] = modeltest.Text("ok")
	}
	m := modeltest.New(steps...)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Complete(context.Background(), agentruntime.ModelRequest{}); err != nil {
				t.Error(err)
			}
			_ = m.Count()
			_ = m.Requests()
			_ = m.Calls()
		}()
	}
	wg.Wait()
	if m.Count() != n || len(m.Requests()) != n || len(m.Calls()) != n {
		t.Fatalf("count = %d", m.Count())
	}
}

func TestModelContextCancelled(t *testing.T) {
	m := modeltest.New(modeltest.Text("a"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Complete(ctx, agentruntime.ModelRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if m.Count() != 0 {
		t.Fatalf("cancelled request must not be recorded, Count = %d", m.Count())
	}
	if resp, err := m.Complete(context.Background(), agentruntime.ModelRequest{}); err != nil || resp.Text != "a" {
		t.Fatalf("step 0 must still be available: %+v, %v", resp, err)
	}
}
