// Package modeltest provides a scripted runtime.Model for tests. Each request
// is answered by the step at the same index and every request is recorded.
package modeltest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
)

var _ agentruntime.Model = (*Model)(nil)

// Usage is the token usage a Step reports. runtime.ModelResponse has no usage
// field yet; for now it is only recorded (see Model.Calls). Hop H6 will copy it
// into the response.
type Usage struct {
	InputTokens, OutputTokens, CachedTokens int64
}

// Step is one scripted answer to a single model request.
type Step struct {
	fn    func(ctx context.Context, index int, req agentruntime.ModelRequest) (agentruntime.ModelResponse, error)
	usage Usage
}

// Text answers with a final text response.
func Text(text string) Step {
	return Step{fn: func(context.Context, int, agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		return agentruntime.ModelResponse{Text: text}, nil
	}}
}

// Call answers with a single tool call. args is marshalled with
// encoding/json unless it is a json.RawMessage, []byte or string, in which
// case it is used verbatim and must be valid JSON (otherwise Complete returns
// an error). nil args become {}. The CallID is "modeltest-call-<request index>".
// The parameter is named alias because later hops send model-facing aliases;
// today it is simply the tool name.
func Call(alias string, args any) Step {
	return Step{fn: func(_ context.Context, index int, _ agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		raw, err := encodeArgs(args)
		if err != nil {
			return agentruntime.ModelResponse{}, fmt.Errorf("modeltest: call %q arguments: %w", alias, err)
		}
		return agentruntime.ModelResponse{ToolCalls: []agentruntime.ToolCall{{
			CallID:    fmt.Sprintf("modeltest-call-%d", index),
			Name:      alias,
			Arguments: raw,
		}}}, nil
	}}
}

// Fail answers with err.
func Fail(err error) Step {
	return Step{fn: func(context.Context, int, agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		return agentruntime.ModelResponse{}, err
	}}
}

// Func is an escape hatch for blocking or conditional steps.
func Func(fn func(context.Context, agentruntime.ModelRequest) (agentruntime.ModelResponse, error)) Step {
	return Step{fn: func(ctx context.Context, _ int, req agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		return fn(ctx, req)
	}}
}

// WithUsage returns a copy of the step that reports u in Model.Calls.
func (s Step) WithUsage(u Usage) Step {
	s.usage = u
	return s
}

func encodeArgs(args any) (json.RawMessage, error) {
	var raw []byte
	switch v := args.(type) {
	case nil:
		return json.RawMessage(`{}`), nil
	case json.RawMessage:
		raw = v
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(b), nil
	}
	if !json.Valid(raw) {
		return nil, errors.New("invalid JSON")
	}
	return json.RawMessage(append([]byte(nil), raw...)), nil
}

// Record is one received request together with how it was answered.
type Record struct {
	Request  agentruntime.ModelRequest
	Response agentruntime.ModelResponse
	Err      error
	Usage    Usage
}

// Model is a scripted runtime.Model. It is safe for concurrent use.
type Model struct {
	mu      sync.Mutex
	steps   []Step
	records []Record
}

// New returns a Model whose i-th request (0-based, across all Complete calls)
// is answered by steps[i].
func New(steps ...Step) *Model {
	return &Model{steps: append([]Step(nil), steps...)}
}

// Then appends further steps and returns the same Model for chaining.
func (m *Model) Then(steps ...Step) *Model {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps = append(m.steps, steps...)
	return m
}

// Complete records req and answers it with the step at the request's index.
// It returns ctx.Err() without recording when ctx is already done. If
// req.OnDispatch is set it is called once before the step is evaluated, as a
// real provider would just before sending; its error is returned. A request
// with no remaining step returns "modeltest: no step for request N" and is
// still recorded.
func (m *Model) Complete(ctx context.Context, req agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return agentruntime.ModelResponse{}, err
	}
	m.mu.Lock()
	index := len(m.records)
	var step Step
	haveStep := index < len(m.steps)
	if haveStep {
		step = m.steps[index]
	}
	stored := req
	stored.OnDispatch = nil
	m.records = append(m.records, Record{Request: stored})
	m.mu.Unlock()

	var (
		resp agentruntime.ModelResponse
		err  error
	)
	if req.OnDispatch != nil {
		err = req.OnDispatch(ctx)
	}
	if err == nil {
		if haveStep {
			resp, err = step.fn(ctx, index, req)
		} else {
			err = fmt.Errorf("modeltest: no step for request %d", index)
		}
	}

	m.mu.Lock()
	m.records[index].Response = resp
	m.records[index].Err = err
	if haveStep {
		m.records[index].Usage = step.usage
	}
	m.mu.Unlock()
	return resp, err
}

// Requests returns copies of every received request in order, with OnDispatch
// set to nil.
func (m *Model) Requests() []agentruntime.ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]agentruntime.ModelRequest, len(m.records))
	for i, r := range m.records {
		out[i] = r.Request
	}
	return out
}

// Calls returns a copy of every received request with its response, error and
// scripted usage.
func (m *Model) Calls() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Record(nil), m.records...)
}

// Count returns the number of requests received.
func (m *Model) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.records)
}
