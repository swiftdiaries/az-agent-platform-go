package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
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
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- app.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := app.Shutdown(shutdownCtx); err != nil {
			t.Error(err)
		}
		if err := <-serveErr; err != nil {
			t.Error(err)
		}
		app.Close()
	}()
	apiURL := "http://" + listener.Addr().String()

	for _, endpoint := range []string{"/livez", "/readyz"} {
		response, err := http.Get(apiURL + endpoint)
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
		req, _ := http.NewRequest(http.MethodPost, apiURL+"/agent", bytes.NewReader(body))
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
	response, err := http.Get(apiURL + "/readyz")
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

func TestServiceShutdownBoundsBlockedAdmissionAndClaim(t *testing.T) {
	for _, barrier := range []struct {
		name    string
		trigger string
	}{
		{"admission", "BEFORE INSERT ON agent_commands"},
		{"claim", "BEFORE UPDATE ON agent_runs FOR EACH ROW WHEN (NEW.state = 'running')"},
	} {
		t.Run(barrier.name, func(t *testing.T) {
			pool := database(t)
			root := t.TempDir()
			registry, err := definitions.Load(writeDefinitionBundle(t, root, "prompt\n"))
			if err != nil {
				t.Fatal(err)
			}
			app, err := service.New(service.Dependencies{
				Pool: pool, Registry: registry, Authenticator: chat.StaticBearerTokens{"token": "alice"},
				Model: modelFunc(func(context.Context, agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
					return agentruntime.ModelResponse{}, nil
				}),
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
			defer func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = app.Shutdown(shutdownCtx)
				<-serveErr
				app.Close()
			}()

			gate, err := pool.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Release()
			if _, err := gate.Exec(t.Context(), "SELECT pg_advisory_lock(784321)"); err != nil {
				t.Fatal(err)
			}
			defer gate.Exec(t.Context(), "SELECT pg_advisory_unlock(784321)")
			if _, err := pool.Exec(t.Context(), `CREATE FUNCTION deployment_shutdown_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(784321); RETURN NEW; END $$;`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), "CREATE TRIGGER deployment_shutdown_barrier "+barrier.trigger+" EXECUTE FUNCTION deployment_shutdown_barrier()"); err != nil {
				t.Fatal(err)
			}

			body, _ := json.Marshal(aguitypes.RunAgentInput{ThreadID: "thread", RunID: barrier.name, Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan"}}})
			request, _ := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/agent", bytes.NewReader(body))
			request.Header.Set("Authorization", "Bearer token")
			postDone := make(chan error, 1)
			go func() {
				response, err := http.DefaultClient.Do(request)
				if response != nil {
					response.Body.Close()
				}
				postDone <- err
			}()
			waitDatabase(t, pool, "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted)")

			beginDone := make(chan struct{})
			go func() { app.BeginDrain(); close(beginDone) }()
			select {
			case <-beginDone:
			case <-time.After(250 * time.Millisecond):
				if _, err := gate.Exec(t.Context(), "SELECT pg_advisory_unlock(784321)"); err != nil {
					t.Fatal(err)
				}
				<-beginDone
				t.Fatal("drain transition waited for blocked submission")
			}
			ready := httptest.NewRecorder()
			app.Handler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if ready.Code != http.StatusServiceUnavailable {
				t.Fatalf("readiness after drain = %d", ready.Code)
			}

			graceCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			shutdownDone := make(chan error, 1)
			go func() { shutdownDone <- app.Shutdown(graceCtx) }()
			select {
			case err := <-shutdownDone:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("shutdown error = %v", err)
				}
			case <-time.After(250 * time.Millisecond):
				if _, err := gate.Exec(t.Context(), "SELECT pg_advisory_unlock(784321)"); err != nil {
					t.Fatal(err)
				}
				<-shutdownDone
				t.Fatal("shutdown exceeded grace while submission was blocked")
			}
			if _, err := gate.Exec(t.Context(), "SELECT pg_advisory_unlock(784321)"); err != nil {
				t.Fatal(err)
			}
			<-postDone

			if _, err := pool.Exec(t.Context(), "UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE state IN ('pending','running')"); err != nil {
				t.Fatal(err)
			}
			if err := journal.New(pool).Reap(t.Context()); err != nil {
				t.Fatal(err)
			}
			var live int
			if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_runs WHERE state IN ('pending','running')").Scan(&live); err != nil || live != 0 {
				t.Fatalf("live run after forced drain = %d, %v", live, err)
			}
			var nonInterrupted int
			if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_runs WHERE state <> 'interrupted'").Scan(&nonInterrupted); err != nil || nonInterrupted != 0 {
				t.Fatalf("blocked receipt disposition = %d non-interrupted runs, %v", nonInterrupted, err)
			}
		})
	}
}
