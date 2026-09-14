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
	"testing"

	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/swiftdiaries/az-agent-platform-go/internal/chat"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	platformmcp "github.com/swiftdiaries/az-agent-platform-go/internal/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/platform"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
)

type modelFunc func(context.Context, agentruntime.ModelRequest) (agentruntime.ModelResponse, error)

func (f modelFunc) Complete(ctx context.Context, req agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
	return f(ctx, req)
}

type mcpObservation struct {
	cookie, authorization, smuggled, callID string
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
	mcp.AddTool(server, &mcp.Tool{Name: "lookup_destination", Description: "look up a destination"},
		func(_ context.Context, req *mcp.CallToolRequest, input struct {
			Destination string `json:"destination"`
		}) (*mcp.CallToolResult, struct {
			Summary string `json:"summary"`
		}, error) {
			mu.Lock()
			observations = append(observations, mcpObservation{
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
	t.Setenv("TEST_JAVA_MCP_ENDPOINT", httpMCP.URL)

	registry, err := definitions.Load(filepath.Join("..", "configs", "journeys.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	model := modelFunc(func(_ context.Context, req agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		if len(req.ToolResults) == 0 {
			if got := toolNames(req.Tools); !slices.Equal(got, []string{"lookup_destination", "request_user_input", "load_skill", "read_skill_resource"}) {
				t.Fatalf("model tools = %v", got)
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
	runner := agentruntime.NewRunner(registry, platformmcp.NewClient(), model)
	pool := database(t)
	service := platform.New(runner, journal.New(pool))
	t.Cleanup(service.Close)
	handler := chat.NewHandler(chat.StaticBearerTokens{"alice-token": "alice", "bob-token": "bob"}, service, pool)
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
	if len(got) != 1 || got[0].cookie != "session=alice" || got[0].authorization != "Bearer alice-token" || got[0].smuggled != "" || got[0].callID == "" || got[0].callID == "provider-call-id" {
		t.Fatalf("MCP observations = %#v", got)
	}
	aliceSession := statefulSessionID(gotTransport, "session=alice", "Bearer alice-token")
	if aliceSession == "" {
		t.Fatalf("stateful streamable session did not reuse a session ID with allowlisted headers: %#v", gotTransport)
	}
	internalThreadID, internalRunID := assertTrace(t, spanRecorder.Ended(), got[0].callID, body.ThreadID, body.RunID, "Plan Kyoto", "session=alice", "alice-token", "Kyoto")
	if internalThreadID == body.ThreadID || internalRunID == body.RunID || internalThreadID == "" || internalRunID == "" {
		t.Fatalf("external identity was not mapped to opaque product IDs: thread=%q run=%q", internalThreadID, internalRunID)
	}
	if !strings.Contains(response.Body, body.ThreadID) || !strings.Contains(response.Body, body.RunID) ||
		strings.Contains(response.Body, internalThreadID) || strings.Contains(response.Body, internalRunID) {
		t.Fatalf("AG-UI response did not preserve external correlation at the edge: %s", response.Body)
	}
	firstSnapshot, err := service.Snapshot(context.Background(), internalThreadID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if callIDFromEvents(firstSnapshot.Events) != got[0].callID {
		t.Fatalf("event/product call ID does not match Java header: %#v", firstSnapshot.Events)
	}

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
	if _, err := service.Snapshot(context.Background(), bobThreadID, "bob"); err != nil {
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
	expiredSnapshot, err := service.Snapshot(context.Background(), expiredThreadID, "alice")
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
	internalSnapshot, err := service.Snapshot(context.Background(), failedThreadID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if internalSnapshot.RunState != platform.RunFailed || !slices.ContainsFunc(internalSnapshot.Events, func(event platform.Event) bool { return event.Type == "run.failed" }) {
		t.Fatalf("internal failure snapshot = %#v", internalSnapshot)
	}
	snapshot, err := service.Snapshot(context.Background(), internalThreadID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RunState != platform.RunCompleted || snapshot.Answer != "Kyoto is available" || len(snapshot.Events) < 4 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if _, err := service.Snapshot(context.Background(), internalThreadID, "mallory"); err == nil {
		t.Fatal("forged principal read another principal's thread")
	}
	if _, err := service.Snapshot(context.Background(), body.ThreadID, "alice"); err == nil {
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
	handler := chat.NewHandler(chat.StaticBearerTokens{"good": "alice"}, nil, nil)
	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)
	res := postAGUI(t, api.URL, aguitypes.RunAgentInput{ThreadID: "t", RunID: "r"}, "", "", "")
	if res.StatusCode != http.StatusUnauthorized || strings.Contains(res.Body, "alice") {
		t.Fatalf("response = %#v", res)
	}
}

func TestToolBindingFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name    string
		tools   []string
		allowed []string
	}{
		{name: "unavailable allowlisted tool", tools: []string{"registered"}, allowed: []string{"missing"}},
		{name: "normalized collision", tools: []string{"a b", "a@b"}, allowed: []string{"a_b"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := mcp.NewServer(&mcp.Implementation{Name: "binding-fixture", Version: "1"}, nil)
			for _, name := range test.tools {
				mcp.AddTool(server, &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
					return nil, struct{}{}, nil
				})
			}
			endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: false}))
			t.Cleanup(endpoint.Close)
			_, err := platformmcp.NewClient().Bind(t.Context(), definitions.MCPServer{ID: "s", Endpoint: endpoint.URL}, test.allowed, nil)
			if err == nil {
				t.Fatal("Bind accepted unsafe tool registry")
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
	_, err := platformmcp.NewClient().Bind(t.Context(), definitions.MCPServer{
		ID: "redirect", Endpoint: redirect.URL, ForwardHeaders: []string{"Authorization", "Cookie"},
	}, []string{"lookup_destination"}, headers)
	if err == nil {
		t.Fatal("Bind followed an MCP redirect")
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

func TestJourneyConfigFailsClosed(t *testing.T) {
	cases := map[string]string{
		"empty tools":       `{"mcp_servers":[{"id":"s","endpoint":"http://example","forward_headers":[],"tools":["t"]}],"journeys":[{"id":"j","description":"x","system_prompt":"p.md","mcp":{"server":"s","tools":[]}}]}`,
		"missing tools":     `{"mcp_servers":[{"id":"s","endpoint":"http://example","forward_headers":[],"tools":["t"]}],"journeys":[{"id":"j","description":"x","system_prompt":"p.md","mcp":{"server":"s"}}]}`,
		"unknown field":     `{"extra":true,"mcp_servers":[],"journeys":[]}`,
		"duplicate journey": `{"mcp_servers":[{"id":"s","endpoint":"http://example","forward_headers":[],"tools":["t"]}],"journeys":[{"id":"j","description":"x","system_prompt":"p.md","mcp":{"server":"s","tools":["t"]}},{"id":"j","description":"x","system_prompt":"p.md","mcp":{"server":"s","tools":["t"]}}]}`,
		"escaping prompt":   `{"mcp_servers":[{"id":"s","endpoint":"http://example","forward_headers":[],"tools":["t"]}],"journeys":[{"id":"j","description":"x","system_prompt":"../p.md","mcp":{"server":"s","tools":["t"]}}]}`,
		"missing prompt":    `{"mcp_servers":[{"id":"s","endpoint":"http://example","forward_headers":[],"tools":["t"]}],"journeys":[{"id":"j","description":"x","system_prompt":"missing.md","mcp":{"server":"s","tools":["t"]}}]}`,
		"undeclared tool":   `{"mcp_servers":[{"id":"s","endpoint":"http://example","forward_headers":[],"tools":["registered"]}],"journeys":[{"id":"j","description":"x","system_prompt":"p.md","mcp":{"server":"s","tools":["registred"]}}]}`,
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := writeFixture(filepath.Join(dir, "p.md"), "fixture prompt"); err != nil {
				t.Fatal(err)
			}
			if err := writeFixture(filepath.Join(dir, "journeys.yaml"), config); err != nil {
				t.Fatal(err)
			}
			if _, err := definitions.Load(filepath.Join(dir, "journeys.yaml")); err == nil {
				t.Fatal("Load accepted invalid config")
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
	wantNames := map[string]bool{"chat.accept": false, "agent.start_turn": false, "hub.route": false, "journey.run": false, "model.call": false, "mcp.discover": false, "mcp.call": false}
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
		"mcp.discover":     "journey.run",
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
