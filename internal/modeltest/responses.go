package modeltest

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"
)

// ResponsesServer is a scripted OpenAI-Responses-compatible httptest server
// acting as an enforcement point. POST <URL>/responses answers request i
// (0-based) with step i; every request is recorded. A request with no step
// gets HTTP 500. Anything other than POST /responses gets 404 (still recorded
// and not consuming a step).
type ResponsesServer struct {
	srv *httptest.Server

	mu      sync.Mutex
	steps   []ResponseStep
	next    int
	records []ResponsesRequest
}

// ResponsesRequest is one recorded request.
type ResponsesRequest struct {
	Method, Path string
	Header       http.Header
	Body         []byte
}

// ResponseStep is one scripted answer.
type ResponseStep struct {
	status int
	header http.Header
	body   string
	output []map[string]any // completed responses only
	usage  *Usage
}

// NewResponsesServer starts the server with the given steps.
func NewResponsesServer(steps ...ResponseStep) *ResponsesServer {
	s := &ResponsesServer{steps: steps}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// URL is the base URL for the openai-go client.
func (s *ResponsesServer) URL() string { return s.srv.URL }

// Close shuts the server down.
func (s *ResponsesServer) Close() { s.srv.Close() }

// Then appends steps.
func (s *ResponsesServer) Then(steps ...ResponseStep) *ResponsesServer {
	s.mu.Lock()
	s.steps = append(s.steps, steps...)
	s.mu.Unlock()
	return s
}

// Requests returns copies of the recorded requests.
func (s *ResponsesServer) Requests() []ResponsesRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ResponsesRequest, len(s.records))
	for i, r := range s.records {
		out[i] = ResponsesRequest{
			Method: r.Method, Path: r.Path,
			Header: r.Header.Clone(), Body: append([]byte(nil), r.Body...),
		}
	}
	return out
}

// Count is the number of recorded requests.
func (s *ResponsesServer) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

func (s *ResponsesServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rec := ResponsesRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body}

	s.mu.Lock()
	s.records = append(s.records, rec)
	if r.Method != http.MethodPost || r.URL.Path != "/responses" {
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	idx := s.next
	s.next++
	if idx >= len(s.steps) {
		s.mu.Unlock()
		writeJSON(w, http.StatusInternalServerError, nil,
			`{"error":{"type":"server_error","message":"modeltest: no scripted response step left"}}`)
		return
	}
	step := s.steps[idx]
	s.mu.Unlock()

	if step.output == nil {
		writeJSON(w, step.status, step.header, step.body)
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)
	var u Usage
	if step.usage != nil {
		u = *step.usage
	}
	resp := map[string]any{
		"id":     fmt.Sprintf("resp_%d", idx),
		"object": "response",
		"status": "completed",
		"model":  req.Model,
		"output": step.output,
		"usage": map[string]any{
			"input_tokens":          u.InputTokens,
			"input_tokens_details":  map[string]any{"cached_tokens": u.CachedTokens},
			"output_tokens":         u.OutputTokens,
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			"total_tokens":          u.InputTokens + u.OutputTokens,
		},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, nil, string(b))
}

func writeJSON(w http.ResponseWriter, status int, header http.Header, body string) {
	for k, vs := range header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// RespondText answers with a completed assistant text message.
func RespondText(text string) ResponseStep {
	return ResponseStep{output: []map[string]any{{
		"type": "message", "id": "msg_0", "role": "assistant", "status": "completed",
		"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
	}}}
}

// RespondCall answers with one function_call output item.
func RespondCall(callID, name string, arguments json.RawMessage) ResponseStep {
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	return ResponseStep{output: []map[string]any{{
		"type": "function_call", "id": "fc_" + callID, "call_id": callID,
		"name": name, "arguments": string(arguments), "status": "completed",
	}}}
}

// RespondBudgetExceeded answers 429 with the agentgateway budget_exceeded body
// and Retry-After (ceil seconds; omitted when 0).
func RespondBudgetExceeded(retryAfter time.Duration) ResponseStep {
	return rateLimit(retryAfter, "budget_exceeded", "budget exceeded")
}

// RespondRateLimited answers 429 with a rate_limit_exceeded body.
func RespondRateLimited(retryAfter time.Duration) ResponseStep {
	return rateLimit(retryAfter, "rate_limit_exceeded", "rate limited")
}

func rateLimit(retryAfter time.Duration, code, msg string) ResponseStep {
	h := http.Header{}
	if secs := int64(math.Ceil(retryAfter.Seconds())); secs > 0 {
		h.Set("Retry-After", strconv.FormatInt(secs, 10))
	}
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"type": "rate_limit_error", "code": code, "message": msg,
	}})
	return ResponseStep{status: http.StatusTooManyRequests, header: h, body: string(b)}
}

// RespondStatus answers with an arbitrary status, headers and body.
func RespondStatus(status int, header http.Header, body string) ResponseStep {
	return ResponseStep{status: status, header: header.Clone(), body: body}
}

// WithUsage sets the completed response's usage.
func (s ResponseStep) WithUsage(u Usage) ResponseStep {
	s.usage = &u
	return s
}
