package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/swiftdiaries/az-agent-platform-go/internal/catalog"
	"github.com/swiftdiaries/az-agent-platform-go/internal/chat"
	"github.com/swiftdiaries/az-agent-platform-go/internal/credential"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/platform"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
)

type modelFunc func(context.Context, agentruntime.ModelRequest) (agentruntime.ModelResponse, error)

func (f modelFunc) Complete(ctx context.Context, req agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
	return f(ctx, req)
}

type mcpObservation struct {
	tool, cookie, authorization, smuggled, callID string
}

type transportObservation struct {
	method, sessionID, cookie, authorization, smuggled string
}

func TestJourneyAuthenticatedAGUIToMCP(t *testing.T) {
	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
	previousTracerProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(tracerProvider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previousTracerProvider)
		_ = tracerProvider.Shutdown(context.Background())
	})

	var mu sync.Mutex
	var observations []mcpObservation
	var transportObservations []transportObservation
	var expiredAttempts int
	server := mcp.NewServer(&mcp.Implementation{Name: "java-fixture", Version: "1"}, nil)
	mcp.AddTool(server, lookupTool("look up a destination"),
		func(_ context.Context, req *mcp.CallToolRequest, input struct {
			Destination string `json:"destination"`
		}) (*mcp.CallToolResult, struct {
			Summary string `json:"summary"`
		}, error) {
			mu.Lock()
			observations = append(observations, mcpObservation{
				tool:   req.Params.Name,
				cookie: req.Extra.Header.Get("Cookie"), authorization: req.Extra.Header.Get("Authorization"),
				smuggled: req.Extra.Header.Get("X-Smuggle"),
				callID:   req.Extra.Header.Get("X-Platform-Call-ID"),
			})
			mu.Unlock()
			return nil, struct {
				Summary string `json:"summary"`
			}{Summary: "Kyoto is available"}, nil
		})
	streamableHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: false})
	httpMCP := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		transportObservations = append(transportObservations, transportObservation{
			method: request.Method, sessionID: request.Header.Get("Mcp-Session-Id"), cookie: request.Header.Get("Cookie"),
			authorization: request.Header.Get("Authorization"), smuggled: request.Header.Get("X-Smuggle"),
		})
		mu.Unlock()
		if request.Header.Get("Cookie") == "session=expired" && request.Method == http.MethodPost {
			payload, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
				return
			}
			request.Body = io.NopCloser(bytes.NewReader(payload))
			if bytes.Contains(payload, []byte(`"method":"tools/call"`)) {
				mu.Lock()
				expiredAttempts++
				mu.Unlock()
				http.Error(writer, "expired downstream credential detail", http.StatusUnauthorized)
				return
			}
		}
		streamableHandler.ServeHTTP(writer, request)
	}))
	t.Cleanup(httpMCP.Close)

	registry := compileSet(t, filepath.Join("..", "configs", "journeys.yaml"))
	model := modelFunc(func(_ context.Context, req agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		if len(req.ToolResults) == 0 {
			// The model sees the alias (never the remote name the server publishes).
			if got := toolNames(req.Tools); !slices.Equal(got, []string{"lookup_destination", "request_user_input", "load_skill", "read_skill_resource"}) {
				t.Fatalf("model tools = %v", got)
			}
			if slices.Contains(req.Messages, "try remote") {
				return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{
					Name: remoteLookup, Arguments: json.RawMessage(`{}`),
				}}, nil
			}
			if slices.Contains(req.Messages, "hello") {
				return agentruntime.ModelResponse{Text: "Hello from the planner"}, nil
			}
			if slices.Contains(req.Messages, "explode") {
				return agentruntime.ModelResponse{}, errors.New("internal database password=bad")
			}
			if slices.Contains(req.Messages, "try hidden") {
				return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{
					Name: "hidden_tool", Arguments: json.RawMessage(`{}`),
				}}, nil
			}
			return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{
				CallID: "provider-call-id", Name: "lookup_destination", Arguments: json.RawMessage(`{"destination":"Kyoto"}`),
			}}, nil
		}
		if req.ToolResults[0].CallID != "provider-call-id" {
			t.Fatalf("provider result correlation ID = %q", req.ToolResults[0].CallID)
		}
		return agentruntime.ModelResponse{Text: "Kyoto is available"}, nil
	})
	runner := agentruntime.NewRunner(registry, model, testTenancy(t, httpMCP.URL))
	pool := database(t)
	service := platform.New(runner, journal.New(pool))
	t.Cleanup(service.Close)
	handler := chat.NewHandler(chat.StaticBearerTokens{"alice-token": testPrincipal, "bob-token": "tenant-a/bob"}, service, pool, []string{"Authorization", "Cookie"})
	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)

	body := aguitypes.RunAgentInput{
		ThreadID: "thread-secret-alice@example.com", RunID: "run-secret-ssn-1234",
		State:    map[string]any{"principal": "mallory", "journey": "vacation-planner"},
		Messages: []aguitypes.Message{{ID: "message-1", Role: aguitypes.RoleUser, Content: "Plan Kyoto"}},
	}
	response := postAGUI(t, api.URL, body, "alice-token", "session=alice", "steal-me")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, response.Body)
	}
	if !strings.Contains(response.Body, "Kyoto is available") {
		t.Fatalf("answer missing from SSE: %s", response.Body)
	}
	if strings.Contains(response.Body, "session=alice") || strings.Contains(response.Body, "steal-me") {
		t.Fatalf("credential leaked in SSE: %s", response.Body)
	}

	mu.Lock()
	got := slices.Clone(observations)
	gotTransport := slices.Clone(transportObservations)
	mu.Unlock()
	if len(got) != 1 || got[0].cookie != "session=alice" || got[0].authorization != "Bearer alice-token" || got[0].smuggled != "" || got[0].callID == "" || got[0].callID == "provider-call-id" || got[0].tool != remoteLookup {
		t.Fatalf("MCP observations = %#v", got)
	}
	aliceSession := statefulSessionID(gotTransport, "session=alice", "Bearer alice-token")
	if aliceSession == "" {
		t.Fatalf("stateful streamable session did not reuse a session ID with allowlisted headers: %#v", gotTransport)
	}
	internalThreadID, internalRunID := assertTrace(t, spanRecorder.Ended(), got[0].callID, body.ThreadID, body.RunID, "Plan Kyoto", "session=alice", "alice-token", "Kyoto", remoteLookup)
	if internalThreadID == body.ThreadID || internalRunID == body.RunID || internalThreadID == "" || internalRunID == "" {
		t.Fatalf("external identity was not mapped to opaque product IDs: thread=%q run=%q", internalThreadID, internalRunID)
	}
	if !strings.Contains(response.Body, body.ThreadID) || !strings.Contains(response.Body, body.RunID) ||
		strings.Contains(response.Body, internalThreadID) || strings.Contains(response.Body, internalRunID) {
		t.Fatalf("AG-UI response did not preserve external correlation at the edge: %s", response.Body)
	}
	firstSnapshot, err := service.Snapshot(context.Background(), internalThreadID, testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if callIDFromEvents(firstSnapshot.Events) != got[0].callID {
		t.Fatalf("event/product call ID does not match Java header: %#v", firstSnapshot.Events)
	}
	for _, event := range firstSnapshot.Events {
		if event.ToolName != "" && event.ToolName != lookupAlias {
			t.Fatalf("event carries tool name %q, want only the alias %q: %#v", event.ToolName, lookupAlias, event)
		}
	}
	requireOperationRows(t, pool, 1)

	bobSpanStart := len(spanRecorder.Ended())
	bobBody := body
	bobBody.Messages = []aguitypes.Message{{ID: body.RunID, Role: aguitypes.RoleUser, Content: "Plan Kyoto"}}
	bobBody.ForwardedProps = map[string]any{"journeyId": "vacation-planner"}
	bob := postAGUI(t, api.URL, bobBody, "bob-token", "session=bob", "other-secret")
	if bob.StatusCode != http.StatusOK {
		t.Fatalf("bob response = %#v", bob)
	}
	bobThreadID, bobRunID := chatIdentity(spanRecorder.Ended()[bobSpanStart:])
	if bobThreadID == "" || bobRunID == "" || bobThreadID == internalThreadID || bobRunID == internalRunID {
		t.Fatalf("principal-scoped identity mapping was not isolated: alice=(%q,%q) bob=(%q,%q)", internalThreadID, internalRunID, bobThreadID, bobRunID)
	}
	if _, err := service.Snapshot(context.Background(), bobThreadID, "tenant-a/bob"); err != nil {
		t.Fatalf("bob snapshot through mapped identity: %v", err)
	}
	mu.Lock()
	got = slices.Clone(observations)
	gotTransport = slices.Clone(transportObservations)
	mu.Unlock()
	if len(got) != 2 || got[1].cookie != "session=bob" || got[1].authorization != "Bearer bob-token" || got[1].smuggled != "" || got[1].callID == got[0].callID {
		t.Fatalf("per-run credential observations = %#v", got)
	}
	bobSession := statefulSessionID(gotTransport, "session=bob", "Bearer bob-token")
	if bobSession == "" || bobSession == aliceSession {
		t.Fatalf("per-run stateful streamable sessions were not isolated: %#v", gotTransport)
	}

	directBody := body
	directBody.ThreadID, directBody.RunID = "thread-3", "message-3"
	directBody.Messages = []aguitypes.Message{{ID: "message-3", Role: aguitypes.RoleUser, Content: "hello"}}
	direct := postAGUI(t, api.URL, directBody, "alice-token", "session=alice-new", "")
	if direct.StatusCode != http.StatusOK || !strings.Contains(direct.Body, "Hello from the planner") {
		t.Fatalf("direct response = %#v", direct)
	}
	mu.Lock()
	callCountAfterDirect := len(observations)
	mu.Unlock()
	if callCountAfterDirect != 2 {
		t.Fatalf("no-tool model response made MCP call, observations = %d", callCountAfterDirect)
	}

	replay := postAGUI(t, api.URL, body, "alice-token", "session=replacement", "")
	if replay.StatusCode != http.StatusOK || !strings.Contains(replay.Body, "Kyoto is available") {
		t.Fatalf("idempotent replay = %#v", replay)
	}
	mu.Lock()
	callCountAfterReplay := len(observations)
	mu.Unlock()
	if callCountAfterReplay != 2 {
		t.Fatalf("stable external ID retry mapping executed MCP again, observations = %d", callCountAfterReplay)
	}
	changed := body
	changed.Messages = []aguitypes.Message{{ID: "message-1", Role: aguitypes.RoleUser, Content: "changed payload"}}
	conflict := postAGUI(t, api.URL, changed, "alice-token", "session=alice", "")
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("changed replay = %#v", conflict)
	}

	hiddenBody := body
	hiddenBody.ThreadID, hiddenBody.RunID = "thread-4", "message-4"
	hiddenBody.Messages = []aguitypes.Message{{ID: "message-4", Role: aguitypes.RoleUser, Content: "try hidden"}}
	hidden := postAGUI(t, api.URL, hiddenBody, "alice-token", "session=alice", "")
	if hidden.StatusCode != http.StatusOK || !strings.Contains(hidden.Body, `"code":"internal_error"`) || strings.Contains(hidden.Body, "hidden_tool") {
		t.Fatalf("unregistered tool response = %#v", hidden)
	}

	remoteBody := body
	remoteBody.ThreadID, remoteBody.RunID = "thread-4b", "message-4b"
	remoteBody.Messages = []aguitypes.Message{{ID: "message-4b", Role: aguitypes.RoleUser, Content: "try remote"}}
	remote := postAGUI(t, api.URL, remoteBody, "alice-token", "session=alice", "")
	if remote.StatusCode != http.StatusOK || !strings.Contains(remote.Body, `"code":"internal_error"`) || strings.Contains(remote.Body, remoteLookup) {
		t.Fatalf("model-requested remote name response = %#v", remote)
	}
	mu.Lock()
	callCountAfterRemote := len(observations)
	mu.Unlock()
	if callCountAfterRemote != 2 {
		t.Fatalf("remote name proposed by the model reached MCP, observations = %d", callCountAfterRemote)
	}

	unknownTarget := body
	unknownTarget.ThreadID, unknownTarget.RunID = "thread-5", "message-5"
	unknownTarget.ForwardedProps = map[string]any{"journeyId": "missing"}
	unknown := postAGUI(t, api.URL, unknownTarget, "alice-token", "session=alice", "")
	if unknown.StatusCode != http.StatusOK || !strings.Contains(unknown.Body, `"code":"internal_error"`) || strings.Contains(unknown.Body, "missing") {
		t.Fatalf("unknown target response = %#v", unknown)
	}

	expiredBody := body
	expiredBody.ThreadID, expiredBody.RunID = "thread-6", "message-6"
	expired := postAGUI(t, api.URL, expiredBody, "alice-token", "session=expired", "")
	if expired.StatusCode != http.StatusOK || !strings.Contains(expired.Body, `"code":"auth_required"`) || strings.Contains(expired.Body, "expired downstream") {
		t.Fatalf("expired downstream auth response = %#v", expired)
	}
	expiredThreadID, _ := latestChatIdentity(spanRecorder.Ended())
	expiredSnapshot, err := service.Snapshot(context.Background(), expiredThreadID, testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if expiredSnapshot.RunState != platform.RunAuthRequired || !slices.ContainsFunc(expiredSnapshot.Events, func(event platform.Event) bool { return event.Type == "auth_required" }) {
		t.Fatalf("expired auth snapshot = %#v", expiredSnapshot)
	}
	mu.Lock()
	gotExpiredAttempts := expiredAttempts
	mu.Unlock()
	if gotExpiredAttempts != 1 {
		t.Fatalf("expired credential tool attempts = %d, want 1", gotExpiredAttempts)
	}

	internalBody := body
	internalBody.ThreadID, internalBody.RunID = "thread-7", "message-7"
	internalBody.Messages = []aguitypes.Message{{ID: "message-7", Role: aguitypes.RoleUser, Content: "explode"}}
	internal := postAGUI(t, api.URL, internalBody, "alice-token", "session=alice", "")
	if internal.StatusCode != http.StatusOK || !strings.Contains(internal.Body, `"code":"internal_error"`) || strings.Contains(internal.Body, "password") {
		t.Fatalf("internal failure response = %#v", internal)
	}
	failedThreadID, _ := latestChatIdentity(spanRecorder.Ended())
	internalSnapshot, err := service.Snapshot(context.Background(), failedThreadID, testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if internalSnapshot.RunState != platform.RunFailed || !slices.ContainsFunc(internalSnapshot.Events, func(event platform.Event) bool { return event.Type == "run.failed" }) {
		t.Fatalf("internal failure snapshot = %#v", internalSnapshot)
	}
	snapshot, err := service.Snapshot(context.Background(), internalThreadID, testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RunState != platform.RunCompleted || snapshot.Answer != "Kyoto is available" || len(snapshot.Events) < 4 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if _, err := service.Snapshot(context.Background(), internalThreadID, "mallory"); err == nil {
		t.Fatal("forged principal read another principal's thread")
	}
	if _, err := service.Snapshot(context.Background(), body.ThreadID, testPrincipal); err == nil {
		t.Fatal("raw external thread ID reached platform state")
	}
}

type httpResult struct {
	StatusCode int
	Body       string
}

func postAGUI(t *testing.T, endpoint string, body aguitypes.RunAgentInput, token, cookie, smuggled string) httpResult {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if smuggled != "" {
		req.Header.Set("X-Smuggle", smuggled)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return httpResult{StatusCode: res.StatusCode, Body: string(data)}
}

func toolNames(tools []agentruntime.ModelTool) []string {
	names := make([]string, len(tools))
	for i := range tools {
		names[i] = tools[i].Name
	}
	return names
}

func TestAuthRejectsMissingCredentials(t *testing.T) {
	handler := chat.NewHandler(chat.StaticBearerTokens{"good": "alice"}, nil, nil, nil)
	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)
	res := postAGUI(t, api.URL, aguitypes.RunAgentInput{ThreadID: "t", RunID: "r"}, "", "", "")
	if res.StatusCode != http.StatusUnauthorized || strings.Contains(res.Body, "alice") {
		t.Fatalf("response = %#v", res)
	}
}

func TestToolBindingFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name  string
		tools []*mcp.Tool
		op    string
	}{
		{name: "missing required operation", tools: []*mcp.Tool{keyedTool("registered", "registered")}, op: "missing"},
		{name: "ambiguous operation", tools: []*mcp.Tool{keyedTool("a1", "a"), keyedTool("a2", "a")}, op: "a"},
		{name: "unkeyed tool is never offered", tools: []*mcp.Tool{{Name: "a", InputSchema: map[string]any{"type": "object"}}}, op: "a"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := mcp.NewServer(&mcp.Implementation{Name: "binding-fixture", Version: "1"}, nil)
			for _, tool := range test.tools {
				mcp.AddTool(server, tool, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
					return nil, struct{}{}, nil
				})
			}
			endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: false}))
			t.Cleanup(endpoint.Close)
			set := compileSet(t, writeBundleConfig(t, t.TempDir(), "prompt\n", test.op, "read_only"))
			binding, err := openTestCatalog(t.Context(), t, testEndpoint(t, endpoint.URL), credential.Credential{}, set)
			if err != nil {
				t.Fatal(err)
			}
			if resolution, ok := binding.Resolution("planner"); !ok || resolution.Available() || len(resolution.Tools) != 0 {
				t.Fatalf("unsafe tool registry resolved: %#v", resolution)
			}
		})
	}
}

func TestToolBindingDoesNotForwardCredentialsAcrossRedirect(t *testing.T) {
	var hostileRequests int
	var hostileAuthorization, hostileCookie string
	hostile := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hostileRequests++
		hostileAuthorization = request.Header.Get("Authorization")
		hostileCookie = request.Header.Get("Cookie")
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hostile.Close)

	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, hostile.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)

	headers := make(http.Header)
	headers.Set("Authorization", "Bearer redirect-secret")
	headers.Set("Cookie", "session=redirect-secret")
	set := compileSet(t, writeDefinitionBundle(t, t.TempDir(), "prompt\n"))
	_, err := openTestCatalog(t.Context(), t, testEndpoint(t, redirect.URL), credential.FromRequest(&http.Request{Header: headers}, []string{"Authorization", "Cookie"}), set)
	if !errors.Is(err, catalog.ErrEndpointUnavailable) {
		t.Fatalf("Open over an MCP redirect = %v, want ErrEndpointUnavailable", err)
	}
	if hostileRequests != 0 || hostileAuthorization != "" || hostileCookie != "" {
		t.Fatalf("redirect destination received requests=%d authorization=%q cookie=%q", hostileRequests, hostileAuthorization, hostileCookie)
	}
}

func statefulSessionID(observations []transportObservation, cookie, authorization string) string {
	var sessionID string
	requestsWithSession := 0
	for _, observation := range observations {
		if observation.cookie != cookie || observation.authorization != authorization {
			continue
		}
		if observation.smuggled != "" {
			return ""
		}
		if observation.sessionID == "" {
			continue
		}
		if sessionID != "" && sessionID != observation.sessionID {
			return ""
		}
		sessionID = observation.sessionID
		requestsWithSession++
	}
	if requestsWithSession < 2 {
		return ""
	}
	return sessionID
}

func replacing(old, new string) func(string) string {
	return func(config string) string { return strings.Replace(config, old, new, 1) }
}
func prepending(text string) func(string) string {
	return func(config string) string { return text + config }
}
func appending(text string) func(string) string {
	return func(config string) string { return config + text }
}

const (
	controlTool    = "      - op: lookup\n"
	controlToolSet = "    tools:\n      - op: lookup\n        class: read_only\n"
)

// journeyConfigCases mutate the valid planner bundle written by writeBundleConfig
// (read-only op "lookup", call_id_header set) so that exactly one fail-closed rule
// is violated, and require the rejection to name that rule. Each edit must change
// the text, so a stale anchor cannot make a case pass vacuously.
var journeyConfigCases = []struct {
	name     string
	edit     func(config string) string
	wantText string
}{
	{"unknown top-level field", prepending("extra: true\n"), "unknown field"},
	{"unknown catalog field", replacing("catalog:\n", "catalog:\n  extra: true\n"), "unknown field"},
	{"unknown journey field", replacing("    skill:", "    mystery: 1\n    skill:"), "unknown field"},
	{"unknown tool field", replacing(controlTool, controlTool+"        mystery: 1\n"), "unknown field"},
	{"trailing document", appending("---\ncatalog:\n  operation_meta_key: operation_key\n"), "exactly one YAML document"},
	{"empty file", func(string) string { return "" }, "empty"},
	{"missing operation key selector", replacing(controlTool+"        class: read_only\n", "      - class: read_only\n"), "exactly one of op and tags"},
	{"op and tags together", replacing(controlTool, controlTool+"        tags: [leave]\n"), "exactly one of op and tags"},
	{"empty operation key", replacing(controlTool, "      - op: \"\"\n"), "empty"},
	{"duplicate operation key", appending(controlTool), "more than once"},
	{"invalid class", replacing("class: read_only", "class: deduplicated"), "invalid class"},
	{"idempotent without call_id_header", func(c string) string {
		return strings.Replace(strings.Replace(c, "  call_id_header: X-Platform-Call-ID\n", "", 1), "class: read_only", "class: idempotent", 1)
	}, "call_id_header"},
	{"invalid approval", replacing("class: read_only", "class: read_only\n        approval: optional"), "approval"},
	{"missing operation_meta_key", replacing("  operation_meta_key: operation_key\n", ""), "operation_meta_key"},
	{"invalid alias", replacing(controlTool, "      - op: 9lives\n"), "alias"},
	{"reserved alias", replacing(controlTool, "      - op: load_skill\n"), "reserved"},
	{"empty tools", replacing(controlToolSet, "    tools: []\n"), "tools must not be empty"},
	{"missing tools", replacing(controlToolSet, ""), "tools must not be empty"},
	{"duplicate journey", func(c string) string { return c + c[strings.Index(c, "  - id: planner"):] }, "duplicate journey id"},
	{"escaping prompt", replacing("system_prompt: prompts/planner.md", "system_prompt: ../planner.md"), "parent components"},
	{"missing prompt", replacing("system_prompt: prompts/planner.md", "system_prompt: prompts/missing.md"), "system_prompt"},
	{"escaping skill path", replacing("skill: skills/planner", "skill: ../planner"), "parent components"},
	{"absolute skill path", replacing("skill: skills/planner", "skill: /etc"), "absolute"},
	{"missing skill bundle", replacing("skill: skills/planner", "skill: skills/absent"), "skill:"},
	{"max_tools out of range", appending("    max_tools: 65\n"), "max_tools"},
}

func TestJourneyConfigFailsClosed(t *testing.T) {
	// Positive control: the unmodified bundle compiles, so every rejection below is
	// caused by its single edit and not by the surrounding fixture.
	controlRoot := t.TempDir()
	controlPath := writeBundleConfig(t, controlRoot, "fixture prompt\n", "lookup", "read_only")
	if _, err := definitions.Compile(controlPath); err != nil {
		t.Fatalf("control config rejected: %v", err)
	}
	control, err := os.ReadFile(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range journeyConfigCases {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := writeBundleConfig(t, root, "fixture prompt\n", "lookup", "read_only")
			edited := test.edit(string(control))
			if edited == string(control) {
				t.Fatal("edit did not change the control config")
			}
			if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := definitions.Compile(path)
			if err == nil {
				t.Fatalf("Compile accepted invalid config:\n%s", edited)
			}
			if test.wantText != "" && !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("rejection %q does not mention %q", err, test.wantText)
			}
		})
	}
}

// MCP URLs come only from the deploy-time allowlist, so a config that still
// carries one (at any level) is rejected as an unknown field rather than
// silently ignored.
func TestJourneyConfigRejectsEndpoint(t *testing.T) {
	load := func(edit func(string) string) error {
		path := writeBundleConfig(t, t.TempDir(), "fixture prompt\n", "lookup", "read_only")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		edited := edit(string(data))
		if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = definitions.Compile(path)
		return err
	}
	if err := load(func(c string) string { return c }); err != nil {
		t.Fatalf("control config without endpoint: %v", err)
	}
	const endpoint = "endpoint: http://attacker.example/mcp\n"
	for name, edit := range map[string]func(string) string{
		"top level": func(c string) string { return endpoint + c },
		"catalog":   func(c string) string { return strings.Replace(c, "catalog:\n", "catalog:\n  "+endpoint, 1) },
		"journey":   func(c string) string { return strings.Replace(c, "    skill:", "    "+endpoint+"    skill:", 1) },
		"selector": func(c string) string {
			return strings.Replace(c, "      - op: lookup\n", "      - op: lookup\n        "+endpoint, 1)
		},
	} {
		err := load(edit)
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("config with endpoint at %s = %v, want unknown-field rejection", name, err)
		}
	}
}

// A journey whose required operation the server does not publish under the
// configured _meta key is unavailable: the run fails without a model call and
// without any tool dispatch (an unkeyed tool of the same name is never offered).
func TestJourneyUnavailableRunFailsWithoutModelCall(t *testing.T) {
	pool := database(t)
	for name, tool := range map[string]*mcp.Tool{
		"wrong operation key": keyedTool(remoteLookup, "some_other_operation"),
		"unkeyed tool":        {Name: lookupAlias, InputSchema: map[string]any{"type": "object"}},
	} {
		t.Run(name, func(t *testing.T) {
			var dispatched, models atomic.Int32
			server := mcp.NewServer(&mcp.Implementation{Name: "unavailable", Version: "1"}, nil)
			mcp.AddTool(server, tool, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
				dispatched.Add(1)
				return nil, struct{}{}, nil
			})
			backend := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
			defer backend.Close()
			p := platform.New(policyRunner(t, backend.URL, "read_only", modelFunc(func(context.Context, agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
				models.Add(1)
				return agentruntime.ModelResponse{Text: "must not be asked"}, nil
			})), journal.New(pool))
			defer p.Close()
			id := "unavailable-" + strings.ReplaceAll(name, " ", "-")
			c := platform.Command{ThreadID: id, RunID: id, CommunicationID: id, Principal: "alice", Tenant: testTenant, Text: "go"}
			if _, err := p.Submit(t.Context(), c, credential.Credential{}); err != nil {
				t.Fatal(err)
			}
			awaitState(t, p, c.ThreadID, c.RunID, journal.RunFailed)
			if models.Load() != 0 || dispatched.Load() != 0 {
				t.Fatalf("unavailable journey made %d model calls and %d dispatches", models.Load(), dispatched.Load())
			}
		})
	}
}

func callIDFromEvents(events []platform.Event) string {
	for _, event := range events {
		if event.Type == "tool.completed" {
			return event.CallID
		}
	}
	return ""
}

func assertTrace(t *testing.T, spans []sdktrace.ReadOnlySpan, callID string, forbidden ...string) (string, string) {
	t.Helper()
	var traceID string
	for _, span := range spans {
		if span.Name() == "chat.accept" {
			traceID = span.SpanContext().TraceID().String()
			break
		}
	}
	if traceID == "" {
		t.Fatal("chat.accept trace not recorded")
	}
	wantNames := map[string]bool{"chat.accept": false, "agent.start_turn": false, "hub.route": false, "journey.run": false, "model.call": false, "catalog.resolve": false, "mcp.call": false}
	spanIDs := make(map[string]string)
	parents := make(map[string]string)
	for _, span := range spans {
		if span.SpanContext().TraceID().String() != traceID {
			continue
		}
		if _, ok := wantNames[span.Name()]; ok {
			wantNames[span.Name()] = true
			spanIDs[span.Name()] = span.SpanContext().SpanID().String()
			parents[span.Name()] = span.Parent().SpanID().String()
		}
		serialized := span.Name()
		for _, value := range span.Attributes() {
			serialized += fmt.Sprint(value.Key, "=", value.Value.Emit())
		}
		for _, secret := range forbidden {
			if strings.Contains(serialized, secret) {
				t.Fatalf("trace leaked %q in %q", secret, serialized)
			}
		}
		if span.Name() == "mcp.call" && spanAttribute(span, "call.id") != callID {
			t.Fatalf("mcp.call call.id = %q, want %q", spanAttribute(span, "call.id"), callID)
		}
		if span.Name() == "mcp.call" && (spanAttribute(span, "tool.alias") != lookupAlias || spanAttribute(span, "operation.key") != lookupAlias) {
			t.Fatalf("mcp.call tool.alias=%q operation.key=%q, want %q for both", spanAttribute(span, "tool.alias"), spanAttribute(span, "operation.key"), lookupAlias)
		}
	}
	for name, found := range wantNames {
		if !found {
			t.Fatalf("trace %s missing span %s", traceID, name)
		}
	}
	for child, parent := range map[string]string{
		"agent.start_turn": "chat.accept",
		"hub.route":        "agent.start_turn",
		"journey.run":      "agent.start_turn",
		"model.call":       "journey.run",
		"catalog.resolve":  "journey.run",
		"mcp.call":         "journey.run",
	} {
		if parents[child] != spanIDs[parent] {
			t.Fatalf("span %s parent = %s, want %s (%s)", child, parents[child], parent, spanIDs[parent])
		}
	}
	for _, span := range spans {
		if span.SpanContext().TraceID().String() == traceID && span.Name() == "chat.accept" {
			return spanAttribute(span, "thread.id"), spanAttribute(span, "run.id")
		}
	}
	t.Fatal("chat.accept identity attributes missing")
	return "", ""
}

func latestChatIdentity(spans []sdktrace.ReadOnlySpan) (string, string) {
	for i := len(spans) - 1; i >= 0; i-- {
		if spans[i].Name() == "chat.accept" {
			return spanAttribute(spans[i], "thread.id"), spanAttribute(spans[i], "run.id")
		}
	}
	return "", ""
}

func chatIdentity(spans []sdktrace.ReadOnlySpan) (string, string) {
	for _, span := range spans {
		if span.Name() == "chat.accept" {
			return spanAttribute(span, "thread.id"), spanAttribute(span, "run.id")
		}
	}
	return "", ""
}

func spanAttribute(span sdktrace.ReadOnlySpan, name string) string {
	for _, value := range span.Attributes() {
		if string(value.Key) == name {
			return value.Value.AsString()
		}
	}
	return ""
}

func writeFixture(path, value string) error {
	return os.WriteFile(path, []byte(value), 0o600)
}
