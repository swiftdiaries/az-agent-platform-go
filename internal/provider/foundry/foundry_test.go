package foundry

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
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/microsoft/agent-framework-go/message"

	platformmcp "github.com/swiftdiaries/az-agent-platform-go/internal/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
)

type credential struct{}

func (credential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "fixture-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestCompleteUsesFoundryProjectResponsesContract(t *testing.T) {
	var dispatched atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/projects/test/openai/v1/responses" {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		if got := request.URL.Query().Get("api-version"); got != "" {
			t.Fatalf("api-version = %q", got)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer fixture-token" {
			t.Fatalf("authorization was not AAD bearer auth")
		}
		body := decodeBody(t, request)
		if body["model"] != "fixture-deployment" || body["store"] != false {
			t.Fatalf("model/store = %#v/%#v", body["model"], body["store"])
		}
		tools, _ := body["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("tools = %#v", body["tools"])
		}
		function := tools[0].(map[string]any)
		if nested, ok := function["function"].(map[string]any); ok {
			function = nested
		}
		if function["name"] != "lookup_destination" {
			t.Fatalf("function = %#v", function)
		}
		if !hasInputTypes(body["input"], "function_call", "function_call_output") {
			t.Fatal("provider request omitted durable tool-call history or result")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","created_at":1741891428,"status":"completed","model":"fixture-deployment","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup_destination","arguments":"{\"city\":\"Kyoto\"}"}]}`)
	}))
	defer server.Close()

	adapter := newAdapter(t, server)
	response, err := adapter.Complete(t.Context(), runtime.ModelRequest{
		Instructions: "Use the selected tool.",
		ProviderMessages: []*message.Message{
			{Role: message.RoleAssistant, Contents: message.Contents{&message.FunctionCallContent{CallID: "previous_call", Name: "lookup_destination", Arguments: `{"city":"Osaka"}`}}},
			{Role: message.RoleTool, Contents: message.Contents{&message.FunctionResultContent{CallID: "previous_call", Result: `available`}}},
			message.NewText("find a destination"),
		},
		Tools:      []runtime.ModelTool{{Name: "lookup_destination", Description: "look up a destination", Schema: map[string]any{"type": "object"}}},
		OnDispatch: func(context.Context) error { dispatched.Add(1); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if dispatched.Load() != 1 || len(response.ToolCalls) != 1 || response.ToolCalls[0].CallID != "call_1" || response.ToolCalls[0].Name != "lookup_destination" || string(response.ToolCalls[0].Arguments) != `{"city":"Kyoto"}` {
		t.Fatalf("dispatch/response = %d/%+v", dispatched.Load(), response)
	}
}

func TestCompleteDoesNotRetryOrLeakProviderError(t *testing.T) {
	const secret = "private-provider-detail"
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, secret, http.StatusInternalServerError)
	}))
	defer server.Close()

	_, err := newAdapter(t, server).Complete(t.Context(), runtime.ModelRequest{ProviderMessages: []*message.Message{message.NewText("secret prompt")}})
	if err == nil || err.Error() != "Foundry model request failed" || strings.Contains(err.Error(), secret) || calls.Load() != 1 {
		t.Fatalf("error/calls = %v/%d", err, calls.Load())
	}
}

func TestCompleteReturnsFinishedText(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_2","object":"response","created_at":1741891428,"status":"completed","model":"fixture-deployment","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"finished","annotations":[]}]}]}`)
	}))
	defer server.Close()

	response, err := newAdapter(t, server).Complete(t.Context(), runtime.ModelRequest{ProviderMessages: []*message.Message{message.NewText("finish")}})
	if err != nil || response.Text != "finished" || len(response.ToolCalls) != 0 {
		t.Fatalf("response/error = %+v/%v", response, err)
	}
}

func TestCompleteSanitizesAuthAndHonorsCancellation(t *testing.T) {
	const secret = "expired-token-detail"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, secret, http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := newAdapter(t, server).Complete(t.Context(), runtime.ModelRequest{ProviderMessages: []*message.Message{message.NewText("secret prompt")}})
	if !errors.Is(err, platformmcp.ErrAuthRequired) || strings.Contains(err.Error(), secret) {
		t.Fatalf("auth error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = newAdapter(t, server).Complete(ctx, runtime.ModelRequest{ProviderMessages: []*message.Message{message.NewText("ignored")}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestConfigFromEnvRequiresNewContract(t *testing.T) {
	_, err := ConfigFromEnv(func(string) string { return "" }, credential{})
	if err == nil || !strings.Contains(err.Error(), EnvProjectEndpoint) {
		t.Fatalf("missing endpoint error = %v", err)
	}
	config, err := ConfigFromEnv(func(name string) string {
		if name == EnvProjectEndpoint {
			return "https://example.projects.ai.azure.com/projects/test"
		}
		return "model-deployment"
	}, credential{})
	if err != nil || config.Deployment != "model-deployment" {
		t.Fatalf("config/error = %+v/%v", config, err)
	}
}

func newAdapter(t *testing.T, server *httptest.Server) *Adapter {
	t.Helper()
	adapter, err := New(Config{ProjectEndpoint: server.URL + "/projects/test", Deployment: "fixture-deployment", Credential: credential{}, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func decodeBody(t *testing.T, request *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func hasInputTypes(raw any, want ...string) bool {
	seen := make(map[string]bool, len(want))
	for _, item := range raw.([]any) {
		if kind, _ := item.(map[string]any)["type"].(string); kind != "" {
			seen[kind] = true
		}
	}
	for _, kind := range want {
		if !seen[kind] {
			return false
		}
	}
	return true
}
