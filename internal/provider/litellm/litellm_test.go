package litellm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/microsoft/agent-framework-go/message"
	platformmcp "github.com/swiftdiaries/az-agent-platform-go/internal/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
)

func TestCompleteSendsStatelessResponsesHistoryAndTools(t *testing.T) {
	var dispatched atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/responses" {
			t.Errorf("request = %s %s", request.Method, request.URL)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer proxy-secret" {
			t.Errorf("authorization = %q", got)
		}
		if dispatched.Load() != 1 {
			t.Errorf("dispatch did not precede send")
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["model"] != "azure-alias" || body["store"] != false || body["instructions"] != "answer carefully" {
			t.Errorf("model/store/instructions = %#v/%#v/%#v", body["model"], body["store"], body["instructions"])
		}
		if _, ok := body["previous_response_id"]; ok {
			t.Error("provider conversation ID sent")
		}
		input := body["input"].([]any)
		if len(input) != 4 {
			t.Errorf("input = %#v", input)
		} else {
			if input[0].(map[string]any)["role"] != "user" || input[0].(map[string]any)["content"] != "find Kyoto" {
				t.Errorf("user input = %#v", input[0])
			}
			call := input[1].(map[string]any)
			if call["type"] != "function_call" || call["call_id"] != "call_older" || call["name"] != "lookup_destination" || call["arguments"] != `{}` {
				t.Errorf("prior call = %#v", call)
			}
			result := input[2].(map[string]any)
			if result["type"] != "function_call_output" || result["call_id"] != "call_older" || result["output"] != `{"city":"Kyoto"}` {
				t.Errorf("prior result = %#v", result)
			}
			if input[3].(map[string]any)["content"] != "continue" {
				t.Errorf("last input = %#v", input[3])
			}
		}
		tools := body["tools"].([]any)
		if len(tools) != 1 {
			t.Errorf("tools = %#v", tools)
		} else {
			tool := tools[0].(map[string]any)
			parameters := tool["parameters"].(map[string]any)
			if tool["type"] != "function" || tool["name"] != "lookup_destination" || tool["description"] != "lookup" || parameters["type"] != "object" || parameters["properties"].(map[string]any)["city"] == nil {
				t.Errorf("tool = %#v", tool)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","created_at":1741891428,"status":"completed","model":"azure-alias","output":[{"type":"function_call","id":"fc_1","call_id":"call_new","name":"lookup_destination","arguments":"{\"city\":\"Osaka\"}"}]}`)
	}))
	defer server.Close()
	adapter := newAdapter(t, server)
	answer, err := adapter.Complete(t.Context(), runtime.ModelRequest{
		Instructions: "answer carefully",
		ProviderMessages: []*message.Message{
			message.NewText("find Kyoto"),
			{Role: message.RoleAssistant, Contents: message.Contents{&message.FunctionCallContent{CallID: "call_older", Name: "lookup_destination", Arguments: `{}`}}},
			{Role: message.RoleTool, Contents: message.Contents{&message.FunctionResultContent{CallID: "call_older", Result: map[string]any{"city": "Kyoto"}}}},
			message.NewText("continue"),
		},
		Tools: []runtime.ModelTool{{Name: "lookup_destination", Description: "lookup", Schema: struct {
			Type       string         `json:"type"`
			Properties map[string]any `json:"properties"`
		}{Type: "object", Properties: map[string]any{"city": map[string]any{"type": "string"}}}}},
		OnDispatch: func(context.Context) error { dispatched.Add(1); return nil },
	})
	if err != nil || dispatched.Load() != 1 || len(answer.ToolCalls) != 1 || answer.ToolCalls[0].CallID != "call_new" || answer.ToolCalls[0].Name != "lookup_destination" || string(answer.ToolCalls[0].Arguments) != `{"city":"Osaka"}` {
		t.Fatalf("dispatch/answer/error = %d/%+v/%v", dispatched.Load(), answer, err)
	}
}

func TestCompleteReturnsFinishedText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_2","object":"response","created_at":1741891428,"status":"completed","model":"azure-alias","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"finished","annotations":[]}]}]}`)
	}))
	defer server.Close()
	answer, err := newAdapter(t, server).Complete(t.Context(), runtime.ModelRequest{ProviderMessages: []*message.Message{message.NewText("finish")}})
	if err != nil || answer.Text != "finished" || len(answer.ToolCalls) != 0 {
		t.Fatalf("answer/error = %+v/%v", answer, err)
	}
}

func TestCompleteDispatchFailureAndSerializationDoNotSend(t *testing.T) {
	var calls, dispatched atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	adapter := newAdapter(t, server)
	sentinel := errors.New("journal materialization failed")
	_, err := adapter.Complete(t.Context(), runtime.ModelRequest{
		ProviderMessages: []*message.Message{message.NewText("dispatch")},
		OnDispatch:       func(context.Context) error { dispatched.Add(1); return sentinel },
	})
	if !errors.Is(err, sentinel) || dispatched.Load() != 1 || calls.Load() != 0 {
		t.Fatalf("dispatch error/calls = %v/%d/%d", err, dispatched.Load(), calls.Load())
	}
	_, err = adapter.Complete(t.Context(), runtime.ModelRequest{
		ProviderMessages: []*message.Message{message.NewText("serialize")},
		Tools:            []runtime.ModelTool{{Name: "bad", Schema: map[string]any{"invalid": make(chan int)}}},
		OnDispatch:       func(context.Context) error { dispatched.Add(1); return nil },
	})
	if err == nil || dispatched.Load() != 1 || calls.Load() != 0 {
		t.Fatalf("serialization error/calls = %v/%d/%d", err, dispatched.Load(), calls.Load())
	}
}

func TestCompleteDoesNotRetryRedirectOrLeakProviderDetails(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.URL.Path == "/redirected" {
			t.Error("redirected request received proxy token")
		}
		http.Redirect(w, request, "/redirected", http.StatusFound)
	}))
	defer server.Close()
	_, err := newAdapter(t, server).Complete(t.Context(), runtime.ModelRequest{ProviderMessages: []*message.Message{message.NewText("redirect")}})
	if err == nil || err.Error() != "LiteLLM model request failed" || calls.Load() != 1 {
		t.Fatalf("redirect error/calls = %v/%d", err, calls.Load())
	}

	var serverErrors atomic.Int32
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serverErrors.Add(1)
		http.Error(w, "private-provider-detail", http.StatusInternalServerError)
	}))
	defer server2.Close()
	_, err = newAdapter(t, server2).Complete(t.Context(), runtime.ModelRequest{ProviderMessages: []*message.Message{message.NewText("secret prompt")}})
	if err == nil || err.Error() != "LiteLLM model request failed" || strings.Contains(err.Error(), "private-provider-detail") || serverErrors.Load() != 1 {
		t.Fatalf("provider error/calls = %v/%d", err, serverErrors.Load())
	}
}

func TestCompleteSanitizesAuthAndPreservesCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "private-auth-detail", http.StatusUnauthorized)
	}))
	defer server.Close()
	adapter := newAdapter(t, server)
	_, err := adapter.Complete(t.Context(), runtime.ModelRequest{ProviderMessages: []*message.Message{message.NewText("secret")}})
	if !errors.Is(err, platformmcp.ErrAuthRequired) || strings.Contains(err.Error(), "private-auth-detail") {
		t.Fatalf("auth error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = adapter.Complete(ctx, runtime.ModelRequest{ProviderMessages: []*message.Message{message.NewText("ignored")}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestConfigRequiresProxyURLKeyAndAlias(t *testing.T) {
	for _, name := range []string{EnvBaseURL, EnvAPIKey, EnvModel} {
		values := map[string]string{EnvBaseURL: "http://localhost:4000", EnvAPIKey: "secret", EnvModel: "azure-alias"}
		delete(values, name)
		if _, err := ConfigFromEnv(func(key string) string { return values[key] }); err == nil {
			t.Errorf("missing %s accepted", name)
		}
	}
	for _, base := range []string{"ftp://example/v1", "https://secret@example/v1", "http://example/v1?key=secret"} {
		if _, err := New(Config{BaseURL: base, APIKey: "secret", Model: "azure-alias"}); err == nil {
			t.Errorf("base URL %q accepted", base)
		}
	}
}

func newAdapter(t *testing.T, server *httptest.Server) *Adapter {
	t.Helper()
	adapter, err := New(Config{BaseURL: server.URL, APIKey: "proxy-secret", Model: "azure-alias", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}
