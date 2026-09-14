package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/chat"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
	"github.com/swiftdiaries/az-agent-platform-go/internal/service"
)

type acceptanceReplica struct {
	app *service.Service
	url string
}

func startAcceptanceReplica(t *testing.T, pool *pgxpool.Pool, registry *definitions.Registry, model agentruntime.Model) acceptanceReplica {
	t.Helper()
	app, err := service.New(service.Dependencies{
		Pool: pool, Registry: registry, Authenticator: chat.StaticBearerTokens{"token": "alice"}, Model: model,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- app.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := app.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Error(err)
		}
		if err := <-serveErr; err != nil {
			t.Error(err)
		}
		app.Close()
	})
	return acceptanceReplica{app: app, url: "http://" + listener.Addr().String()}
}

func acceptancePost(t *testing.T, endpoint string, input aguitypes.RunAgentInput) *http.Response {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func acceptanceReconnect(t *testing.T, endpoint, thread, run string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, endpoint+"?threadId="+thread+"&runId="+run, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func acceptanceRegistry(t *testing.T, endpoint, policy string) *definitions.Registry {
	t.Helper()
	path := writeDefinitionBundle(t, t.TempDir(), "prompt\n")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("http://example"), []byte(endpoint), 1)
	data = bytes.Replace(data, []byte(`"class":"read_only"`), []byte(`"class":"`+policy+`"`), 1)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	registry, err := definitions.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func acceptanceThread(t *testing.T, pool *pgxpool.Pool, external string) string {
	t.Helper()
	var thread string
	if err := pool.QueryRow(t.Context(), "SELECT id FROM chat_threads WHERE principal='alice' AND external_id=$1", external).Scan(&thread); err != nil {
		t.Fatal(err)
	}
	return thread
}

// This is the missing process-level regression: all ingress and reconnects use
// separate Service instances rather than directly wiring Chat to Platform.
func TestAcceptanceThreeServiceHTTPSteeringReconnectAndForcedDrain(t *testing.T) {
	pool := database(t)
	toolStarted, toolRelease := make(chan struct{}), make(chan struct{})
	var releaseTool sync.Once
	t.Cleanup(func() { releaseTool.Do(func() { close(toolRelease) }) })
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "acceptance", Version: "1"}, nil)
	mcp.AddTool(mcpServer, &mcp.Tool{Name: "lookup"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		close(toolStarted)
		select {
		case <-toolRelease:
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "tool result"}}}, struct{}{}, nil
		case <-ctx.Done():
			return nil, struct{}{}, ctx.Err()
		}
	})
	mcpAPI := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil))
	defer mcpAPI.Close()

	lossStarted, lossCancelled := make(chan struct{}), make(chan struct{})
	var modelCalls atomic.Int32
	model := modelFunc(func(ctx context.Context, request agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		modelCalls.Add(1)
		if strings.Contains(strings.Join(request.Messages, "\n"), "force loss") {
			close(lossStarted)
			<-ctx.Done()
			close(lossCancelled)
			return agentruntime.ModelResponse{}, ctx.Err()
		}
		if len(request.ToolResults) == 0 {
			return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{Name: "lookup", Arguments: json.RawMessage(`{}`)}}, nil
		}
		if len(request.PendingCommands) != 1 || request.PendingCommands[0].Text != "use Osaka instead" {
			return agentruntime.ModelResponse{}, errors.New("steering was not delivered at the next provider boundary")
		}
		return agentruntime.ModelResponse{Text: "corrected Osaka"}, nil
	})
	registry := acceptanceRegistry(t, mcpAPI.URL, "read_only")
	a := startAcceptanceReplica(t, pool, registry, model)
	b := startAcceptanceReplica(t, pool, registry, model)
	c := startAcceptanceReplica(t, pool, registry, model)

	first := acceptancePost(t, a.url, aguitypes.RunAgentInput{ThreadID: "steer-thread", RunID: "run-a", Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan"}}})
	defer first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first admission = %d", first.StatusCode)
	}
	select {
	case <-toolStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not start")
	}
	steering := acceptancePost(t, b.url, aguitypes.RunAgentInput{ThreadID: "steer-thread", RunID: "run-b", Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "use Osaka instead"}}})
	defer steering.Body.Close()
	if steering.StatusCode != http.StatusOK {
		t.Fatalf("steering admission = %d", steering.StatusCode)
	}
	reconnect := acceptanceReconnect(t, c.url, "steer-thread", "run-b")
	defer reconnect.Body.Close()
	if reconnect.StatusCode != http.StatusOK {
		t.Fatalf("reconnect = %d", reconnect.StatusCode)
	}
	thread := acceptanceThread(t, pool, "steer-thread")
	snapshot, err := journal.New(pool).Snapshot(t.Context(), thread, "alice")
	if err != nil || len(snapshot.Runs) != 1 || snapshot.Runs[0].PendingCommands != 1 {
		t.Fatalf("durable steering receipt = %#v, %v", snapshot, err)
	}
	releaseTool.Do(func() { close(toolRelease) })
	for name, response := range map[string]*http.Response{"owner": first, "steering": steering, "reconnect": reconnect} {
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body, []byte("corrected Osaka")) || !bytes.Contains(body, []byte("RUN_FINISHED")) {
			t.Fatalf("%s response lost completion: %s", name, body)
		}
	}
	if modelCalls.Load() != 2 {
		t.Fatalf("model calls = %d", modelCalls.Load())
	}

	forced := acceptancePost(t, a.url, aguitypes.RunAgentInput{ThreadID: "loss-thread", RunID: "run-loss", Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "force loss"}}})
	defer forced.Body.Close()
	if forced.StatusCode != http.StatusOK {
		t.Fatalf("loss admission = %d", forced.StatusCode)
	}
	select {
	case <-lossStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("loss run did not start")
	}
	a.app.BeginDrain()
	readiness, err := http.Get(a.url + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	readiness.Body.Close()
	if readiness.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("draining readiness = %d", readiness.StatusCode)
	}
	rejected := acceptancePost(t, a.url, aguitypes.RunAgentInput{ThreadID: "rejected-thread", RunID: "rejected-run", Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "new work"}}})
	rejected.Body.Close()
	if rejected.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("draining admission = %d", rejected.StatusCode)
	}
	drainCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := a.app.Drain(drainCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain = %v", err)
	}
	select {
	case <-lossCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("grace expiry did not cancel local work")
	}
	if _, err := pool.Exec(t.Context(), "UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=(SELECT id FROM chat_runs WHERE external_id='run-loss')"); err != nil {
		t.Fatal(err)
	}
	if err := journal.New(pool).Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	var state journal.RunState
	if err := pool.QueryRow(t.Context(), "SELECT state FROM agent_runs WHERE id=(SELECT id FROM chat_runs WHERE external_id='run-loss')").Scan(&state); err != nil || state != journal.RunInterrupted {
		t.Fatalf("forced loss state = %q, %v", state, err)
	}
}

func TestAcceptanceThreeServiceHTTPApprovalAndReconnect(t *testing.T) {
	pool := database(t)
	var calls atomic.Int32
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "approval", Version: "1"}, nil)
	mcp.AddTool(mcpServer, &mcp.Tool{Name: "lookup"}, func(_ context.Context, request *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, map[string]any, error) {
		authorization := request.Extra.Header.Get("Authorization")
		if authorization != "Bearer token" {
			return nil, nil, errors.New("approval did not use fresh request credentials")
		}
		calls.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "approved tool result"}}}, nil, nil
	})
	mcpAPI := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil))
	defer mcpAPI.Close()
	model := modelFunc(func(_ context.Context, request agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		if len(request.ToolResults) == 0 {
			return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{Name: "lookup", Arguments: json.RawMessage(`{"amount":1}`)}}, nil
		}
		return agentruntime.ModelResponse{Text: "approved and complete"}, nil
	})
	registry := acceptanceRegistry(t, mcpAPI.URL, "effectful")
	a := startAcceptanceReplica(t, pool, registry, model)
	b := startAcceptanceReplica(t, pool, registry, model)
	c := startAcceptanceReplica(t, pool, registry, model)

	wait := acceptancePost(t, a.url, aguitypes.RunAgentInput{ThreadID: "approval-thread", RunID: "approval-run", Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "change"}}})
	defer wait.Body.Close()
	if wait.StatusCode != http.StatusOK {
		t.Fatalf("approval request = %d", wait.StatusCode)
	}
	waitBody, err := io.ReadAll(wait.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(waitBody, []byte("interaction.requested")) {
		t.Fatalf("missing approval request: %s", waitBody)
	}
	thread := acceptanceThread(t, pool, "approval-thread")
	snapshot, err := journal.New(pool).Snapshot(t.Context(), thread, "alice")
	if err != nil || len(snapshot.Runs) != 1 || snapshot.Runs[0].Interaction == nil {
		t.Fatalf("durable approval wait = %#v, %v", snapshot, err)
	}
	interaction := snapshot.Runs[0].Interaction
	reply := acceptancePost(t, b.url, aguitypes.RunAgentInput{
		ThreadID: "approval-thread", RunID: "approval-reply",
		ForwardedProps: map[string]any{"interactionReply": map[string]any{
			"interactionId": interaction.ID, "kind": "approval",
			"answer": map[string]any{"decision": "approve", "binding": interaction.Call.Binding},
		}},
	})
	defer reply.Body.Close()
	if reply.StatusCode != http.StatusOK {
		t.Fatalf("approval reply = %d", reply.StatusCode)
	}
	replyBody, err := io.ReadAll(reply.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(replyBody, []byte("approved and complete")) || calls.Load() != 1 {
		t.Fatalf("approval completion = %s, calls=%d", replyBody, calls.Load())
	}
	reconnect := acceptanceReconnect(t, c.url, "approval-thread", "approval-reply")
	defer reconnect.Body.Close()
	reconnectBody, err := io.ReadAll(reconnect.Body)
	if err != nil {
		t.Fatal(err)
	}
	if reconnect.StatusCode != http.StatusOK || !bytes.Contains(reconnectBody, []byte("approved and complete")) || calls.Load() != 1 {
		t.Fatalf("approval reconnect = %d %s calls=%d", reconnect.StatusCode, reconnectBody, calls.Load())
	}
}
