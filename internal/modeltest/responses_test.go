package modeltest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

func newResponsesClient(s *ResponsesServer, opts ...option.RequestOption) *openai.Client {
	base := []option.RequestOption{
		option.WithBaseURL(s.URL()), option.WithAPIKey("k"), option.WithMaxRetries(0),
	}
	c := openai.NewClient(append(base, opts...)...)
	return &c
}

func newParams(model string) responses.ResponseNewParams {
	return responses.ResponseNewParams{
		Model: model,
		Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("hi")},
	}
}

func TestResponsesServerText(t *testing.T) {
	s := NewResponsesServer(RespondText("hello there"))
	defer s.Close()
	resp, err := newResponsesClient(s).Responses.New(context.Background(), newParams("m1"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.OutputText() != "hello there" {
		t.Fatalf("OutputText = %q", resp.OutputText())
	}
	if resp.Status != responses.ResponseStatusCompleted {
		t.Fatalf("status = %q", resp.Status)
	}
	if resp.ID != "resp_0" {
		t.Fatalf("id = %q", resp.ID)
	}
}

func TestResponsesServerFunctionCall(t *testing.T) {
	s := NewResponsesServer(RespondCall("call_1", "lookup", json.RawMessage(`{"q":"x"}`)))
	defer s.Close()
	resp, err := newResponsesClient(s).Responses.New(context.Background(), newParams("m1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) != 1 || resp.Output[0].Type != "function_call" {
		t.Fatalf("output = %+v", resp.Output)
	}
	fc := resp.Output[0].AsFunctionCall()
	if fc.CallID != "call_1" || fc.Name != "lookup" || fc.Arguments != `{"q":"x"}` {
		t.Fatalf("call = %+v", fc)
	}
}

func TestResponsesServerUsage(t *testing.T) {
	s := NewResponsesServer(RespondText("ok").WithUsage(Usage{InputTokens: 100, OutputTokens: 20, CachedTokens: 40}))
	defer s.Close()
	resp, err := newResponsesClient(s).Responses.New(context.Background(), newParams("gpt-test"))
	if err != nil {
		t.Fatal(err)
	}
	u := resp.Usage
	if u.InputTokens != 100 || u.OutputTokens != 20 || u.InputTokensDetails.CachedTokens != 40 || u.TotalTokens != 120 {
		t.Fatalf("usage = %+v", u)
	}
	if resp.Model != "gpt-test" {
		t.Fatalf("model = %q", resp.Model)
	}
}

func TestResponsesServerRecordsRequests(t *testing.T) {
	s := NewResponsesServer(RespondText("a"), RespondText("b"))
	defer s.Close()
	c := newResponsesClient(s, option.WithHeader("X-Custom", "v1"))
	for i := 0; i < 2; i++ {
		if _, err := c.Responses.New(context.Background(), newParams("m1")); err != nil {
			t.Fatal(err)
		}
	}
	reqs := s.Requests()
	if s.Count() != 2 || len(reqs) != 2 {
		t.Fatalf("count = %d, reqs = %d", s.Count(), len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/responses" {
		t.Fatalf("req = %s %s", r.Method, r.Path)
	}
	if r.Header.Get("X-Custom") != "v1" {
		t.Fatalf("headers = %v", r.Header)
	}
	if !strings.Contains(string(r.Body), `"model":"m1"`) {
		t.Fatalf("body = %s", r.Body)
	}
	r.Header.Set("X-Custom", "mutated")
	if s.Requests()[0].Header.Get("X-Custom") != "v1" {
		t.Fatal("Requests did not return copies")
	}
}

func TestResponsesServerBudgetExceeded(t *testing.T) {
	s := NewResponsesServer(RespondBudgetExceeded(90 * time.Second))
	defer s.Close()
	_, err := newResponsesClient(s).Responses.New(context.Background(), newParams("m1"))
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v", err)
	}
	if apiErr.StatusCode != 429 {
		t.Fatalf("status = %d", apiErr.StatusCode)
	}
	if got := apiErr.Response.Header.Get("Retry-After"); got != "90" {
		t.Fatalf("Retry-After = %q", got)
	}
	if !strings.Contains(apiErr.RawJSON(), "budget_exceeded") {
		t.Fatalf("raw = %s", apiErr.RawJSON())
	}
}

func TestResponsesServerRateLimited(t *testing.T) {
	s := NewResponsesServer(RespondRateLimited(0))
	defer s.Close()
	_, err := newResponsesClient(s).Responses.New(context.Background(), newParams("m1"))
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 429 {
		t.Fatalf("err = %v", err)
	}
	if apiErr.Response.Header.Get("Retry-After") != "" {
		t.Fatal("Retry-After should be omitted for zero")
	}
	if !strings.Contains(apiErr.RawJSON(), "rate_limit_exceeded") {
		t.Fatalf("raw = %s", apiErr.RawJSON())
	}
}

func TestResponsesServerNoStepLeft(t *testing.T) {
	s := NewResponsesServer()
	defer s.Close()
	_, err := newResponsesClient(s).Responses.New(context.Background(), newParams("m1"))
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 500 {
		t.Fatalf("err = %v", err)
	}
	// request 0 consumed index 0, so the next request is answered by step 1
	s.Then(RespondText("unused"), RespondText("late"))
	resp, err := newResponsesClient(s).Responses.New(context.Background(), newParams("m1"))
	if err != nil || resp.OutputText() != "late" {
		t.Fatalf("resp = %v err = %v", resp, err)
	}
}

func TestResponsesServerRespondStatus(t *testing.T) {
	s := NewResponsesServer(RespondStatus(503, http.Header{"X-Why": {"down"}}, `{"error":{"message":"down"}}`))
	defer s.Close()
	_, err := newResponsesClient(s).Responses.New(context.Background(), newParams("m1"))
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 503 || apiErr.Response.Header.Get("X-Why") != "down" {
		t.Fatalf("err = %v", err)
	}
}

func TestResponsesServerOtherPathIs404(t *testing.T) {
	s := NewResponsesServer(RespondText("x"))
	defer s.Close()
	resp, err := http.Get(s.URL() + "/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	reqs := s.Requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].Path != "/models" {
		t.Fatalf("reqs = %+v", reqs)
	}
	// the 404 request must not consume the scripted step
	if r, err := newResponsesClient(s).Responses.New(context.Background(), newParams("m")); err != nil || r.OutputText() != "x" {
		t.Fatalf("resp = %v err = %v", r, err)
	}
}
