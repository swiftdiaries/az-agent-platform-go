package litellm

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/microsoft/agent-framework-go/message"

	"github.com/swiftdiaries/az-agent-platform-go/internal/cost"
	"github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
	"github.com/swiftdiaries/az-agent-platform-go/internal/settings"
)

const budgetBody = `{"error":{"type":"rate_limit_error","code":"budget_exceeded","message":"budget exceeded SECRETBODY"}}`

func limitServer(t *testing.T, calls *atomic.Int32, header http.Header, body string) *httptest.Server {
	t.Helper()
	return statusServer(t, http.StatusTooManyRequests, calls, header, body)
}

func statusServer(t *testing.T, status int, calls *atomic.Int32, header http.Header, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		for k, v := range header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func complete(t *testing.T, server *httptest.Server, request runtime.ModelRequest) (runtime.ModelResponse, error) {
	t.Helper()
	request.ProviderMessages = []*message.Message{message.NewText("hello")}
	return newAdapter(t, server).Complete(t.Context(), request)
}

func TestLimitBudgetExceeded(t *testing.T) {
	var calls atomic.Int32
	server := limitServer(t, &calls, http.Header{"Retry-After": {"30"}, "X-Secret-Header": {"SECRETHEADER"}}, budgetBody)
	_, err := complete(t, server, runtime.ModelRequest{})
	var limit *cost.LimitError
	if !errors.As(err, &limit) || !errors.Is(err, cost.ErrLimitExceeded) {
		t.Fatalf("error = %v", err)
	}
	if limit.Kind != cost.LimitBudget || limit.RetryAfter != 30*time.Second {
		t.Fatalf("limit = %+v", limit)
	}
	if calls.Load() != 1 {
		t.Fatalf("http requests = %d", calls.Load())
	}
	for _, leak := range []string{"SECRETBODY", "SECRETHEADER", "budget exceeded"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error %q leaks %q", err, leak)
		}
	}
}

const liteLLM400Body = `{"error":{"message":"Budget has been exceeded SECRETBODY","type":"budget_exceeded","param":null,"code":"400"}}`

func TestLimit400BudgetExceeded(t *testing.T) {
	var calls atomic.Int32
	server := statusServer(t, http.StatusBadRequest, &calls, http.Header{"Retry-After": {"30"}, "X-Secret-Header": {"SECRETHEADER"}}, liteLLM400Body)
	_, err := complete(t, server, runtime.ModelRequest{})
	var limit *cost.LimitError
	if !errors.As(err, &limit) || !errors.Is(err, cost.ErrLimitExceeded) {
		t.Fatalf("error = %v", err)
	}
	if limit.Kind != cost.LimitBudget || limit.RetryAfter != 30*time.Second {
		t.Fatalf("limit = %+v", limit)
	}
	if calls.Load() != 1 {
		t.Fatalf("http requests = %d", calls.Load())
	}
	for _, leak := range []string{"SECRETBODY", "SECRETHEADER", "Budget has been exceeded"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error %q leaks %q", err, leak)
		}
	}
}

func TestLimit400PlainIsNotLimit(t *testing.T) {
	for name, body := range map[string]string{
		"json": `{"error":{"message":"bad SECRETBODY","type":"invalid_request_error"}}`,
		"text": "bad SECRETBODY",
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := statusServer(t, http.StatusBadRequest, &calls, nil, body)
			_, err := complete(t, server, runtime.ModelRequest{})
			var limit *cost.LimitError
			if err == nil || errors.As(err, &limit) || errors.Is(err, cost.ErrLimitExceeded) {
				t.Fatalf("error = %v", err)
			}
			if calls.Load() != 1 || strings.Contains(err.Error(), "SECRETBODY") {
				t.Fatalf("calls/error = %d/%v", calls.Load(), err)
			}
		})
	}
}

func TestLimitRateLimited(t *testing.T) {
	for name, body := range map[string]string{
		"json": `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`,
		"text": "slow down",
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := limitServer(t, &calls, nil, body)
			_, err := complete(t, server, runtime.ModelRequest{})
			var limit *cost.LimitError
			if !errors.As(err, &limit) || limit.Kind != cost.LimitRate || limit.RetryAfter != 0 {
				t.Fatalf("error = %v", err)
			}
			if calls.Load() != 1 || strings.Contains(err.Error(), "slow down") {
				t.Fatalf("calls/error = %d/%v", calls.Load(), err)
			}
		})
	}
}

func TestAccessAppliedToWire(t *testing.T) {
	names, err := cost.NewHeaders(cost.DefaultHeaderNames())
	if err != nil {
		t.Fatal(err)
	}
	access, err := names.Access(t.Context(), cost.Attribution{Tenant: "t1", Principal: "p1", Conversation: "c1", Run: "r1", Journey: "j1"}, settingsCost())
	if err != nil {
		t.Fatal(err)
	}
	header := captureHeaders(t, runtime.ModelRequest{Access: access})
	for k, want := range map[string]string{"X-Agent-Tenant": "t1", "X-Agent-Principal": "p1", "X-Agent-Conversation": "c1", "X-Agent-Run": "r1", "X-Agent-Journey": "j1"} {
		if got := header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if got := header.Get("Authorization"); got != wantAuthorization {
		t.Errorf("Authorization = %q", got)
	}
}

func TestAccessZeroAddsNothing(t *testing.T) {
	header := captureHeaders(t, runtime.ModelRequest{})
	for k := range header {
		if strings.HasPrefix(strings.ToLower(k), "x-agent-") {
			t.Errorf("unexpected header %s", k)
		}
	}
	if got := header.Get("Authorization"); got != wantAuthorization {
		t.Errorf("Authorization = %q", got)
	}
}

func captureHeaders(t *testing.T, request runtime.ModelRequest) http.Header {
	t.Helper()
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, completedBody(0, 0, 0))
	}))
	defer server.Close()
	if _, err := complete(t, server, request); err != nil {
		t.Fatal(err)
	}
	return seen
}

func TestUsageCopiedToResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, completedBody(120, 34, 50))
	}))
	defer server.Close()
	response, err := complete(t, server, runtime.ModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := cost.Tokens{Model: wantModel, Input: 120, Output: 34, Cached: 50}
	if response.Usage != want {
		t.Fatalf("usage = %+v, want %+v", response.Usage, want)
	}
}

const (
	wantAuthorization = "Bearer proxy-secret"
	wantModel         = "azure-alias"
)

func settingsCost() settings.Cost { return settings.Cost{Profile: "gold"} }

func completedBody(in, out, cached int) string {
	return fmt.Sprintf(`{"id":"resp_1","object":"response","created_at":1741891428,"status":"completed","model":"azure-alias","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d,"input_tokens_details":{"cached_tokens":%d},"output_tokens_details":{"reasoning_tokens":0}}}`, in, out, in+out, cached)
}
