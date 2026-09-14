package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/chat"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
	"github.com/swiftdiaries/az-agent-platform-go/internal/service"
)

func TestServiceDrainRejectsNewAdmissionAndInterruptsExpiredOwner(t *testing.T) {
	pool := database(t)
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "deployment", Version: "1"}, nil)
	mcp.AddTool(mcpServer, &mcp.Tool{Name: "lookup", Description: "read-only lookup"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		return &mcp.CallToolResult{}, struct{}{}, nil
	})
	mcpHandler := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil))
	defer mcpHandler.Close()

	root := t.TempDir()
	path := writeDefinitionBundle(t, root, "prompt\n")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), "http://example", mcpHandler.URL, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	registry, err := definitions.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	cancelled := make(chan struct{})
	model := modelFunc(func(ctx context.Context, _ agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return agentruntime.ModelResponse{}, ctx.Err()
	})
	app, err := service.New(service.Dependencies{
		Pool:          pool,
		Registry:      registry,
		Authenticator: chat.StaticBearerTokens{"token": "alice"},
		Model:         model,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	api := httptest.NewServer(app.Handler())
	defer api.Close()

	for _, endpoint := range []string{"/livez", "/readyz"} {
		response, err := http.Get(api.URL + endpoint)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s before drain = %d", endpoint, response.StatusCode)
		}
	}
	post := func(run string) *http.Response {
		t.Helper()
		body, _ := json.Marshal(aguitypes.RunAgentInput{ThreadID: "thread", RunID: run, Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan"}}})
		req, _ := http.NewRequest(http.MethodPost, api.URL+"/agent", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer token")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	first := post("run-a")
	defer first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first admission = %d", first.StatusCode)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}

	app.BeginDrain()
	response, err := http.Get(api.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readiness during drain = %d", response.StatusCode)
	}
	second := post("run-b")
	second.Body.Close()
	if second.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("new admission during drain = %d", second.StatusCode)
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := app.Drain(drainCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain error = %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("grace expiry did not cancel owned work")
	}
	if _, err := pool.Exec(t.Context(), "UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=(SELECT id FROM chat_runs WHERE external_id='run-a')"); err != nil {
		t.Fatal(err)
	}
	store := journal.New(pool)
	if err := store.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	var state journal.RunState
	if err := pool.QueryRow(t.Context(), "SELECT state FROM agent_runs WHERE id=(SELECT id FROM chat_runs WHERE external_id='run-a')").Scan(&state); err != nil || state != journal.RunInterrupted {
		t.Fatalf("forced drain state = %q, %v", state, err)
	}
}
