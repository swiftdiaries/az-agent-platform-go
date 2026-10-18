package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/swiftdiaries/az-agent-platform-go/internal/credential"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/mcptest"
	"github.com/swiftdiaries/az-agent-platform-go/internal/platform"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
	"github.com/swiftdiaries/az-agent-platform-go/internal/settings"
	"github.com/swiftdiaries/az-agent-platform-go/internal/tenantdir"
)

// awaitTerminal polls the durable snapshot until the run reaches a terminal state.
func awaitTerminal(t *testing.T, store *journal.Store, thread, principal, run string) journal.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := store.Snapshot(t.Context(), thread, principal)
		if err == nil {
			for _, r := range snapshot.Runs {
				switch r.RunID {
				case run:
					switch r.State {
					case journal.RunCompleted, journal.RunFailed, journal.RunAuthRequired, journal.RunDependencyUnavailable:
						return r
					}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach a terminal state", run)
	return journal.Run{}
}

func TestRunScopeRecordedAndDependencyUnavailable(t *testing.T) {
	newServer := func(t *testing.T) *mcptest.Server {
		t.Helper()
		return mcptest.New(t, mcptest.Options{Name: "tenancy-mcp", Version: "v9"}, mcptest.Catalog{Tools: []mcptest.Tool{
			{Name: "lookup", OperationKey: "travel.lookup", Result: "Kyoto is available"},
		}})
	}
	// scripted: one tool call, then an answer; counts every model request.
	scripted := func(models *atomic.Int32) agentruntime.Model {
		return modelFunc(func(_ context.Context, request agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
			models.Add(1)
			if len(request.ToolResults) == 0 {
				return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{Name: "travel_lookup", Arguments: json.RawMessage(`{}`)}}, nil
			}
			return agentruntime.ModelResponse{Text: "done"}, nil
		})
	}
	// submit runs one command for testTenant/testPrincipal and returns its terminal run.
	submit := func(t *testing.T, tenancy agentruntime.Tenancy, models *atomic.Int32, tenant string) journal.Run {
		t.Helper()
		pool := database(t)
		store := journal.New(pool)
		p := platform.New(agentruntime.NewRunner(acceptanceSet(t, "read_only"), scripted(models), tenancy), store)
		t.Cleanup(p.Close)
		c := platform.Command{ThreadID: "tenancy", RunID: "tenancy", CommunicationID: "tenancy", Principal: testPrincipal, Tenant: tenant, Text: "plan"}
		if _, err := p.Submit(t.Context(), c, credential.Credential{}); err != nil {
			t.Fatal(err)
		}
		return awaitTerminal(t, store, c.ThreadID, c.Principal, c.RunID)
	}
	wantUnavailable := func(t *testing.T, run journal.Run, models *atomic.Int32, servers ...*mcptest.Server) {
		t.Helper()
		if run.State != journal.RunDependencyUnavailable {
			t.Fatalf("state = %q, want dependency_unavailable", run.State)
		}
		if got := models.Load(); got != 0 {
			t.Fatalf("model calls = %d, want 0", got)
		}
		for _, server := range servers {
			if got := len(server.Requests()); got != 0 {
				t.Fatalf("MCP server received %d requests, want 0: %+v", got, server.Requests())
			}
		}
	}

	t.Run("happy path records scope, settings and server info", func(t *testing.T) {
		server := newServer(t)
		var models atomic.Int32
		run := submit(t, testTenancy(t, server.URL()), &models, testTenant)
		if run.State != journal.RunCompleted {
			t.Fatalf("state = %q, want completed", run.State)
		}
		if run.ProductVersion != "vN" || run.EndpointID != testEndpointID {
			t.Fatalf("scope = %q %q, want vN %s", run.ProductVersion, run.EndpointID, testEndpointID)
		}
		var recorded struct {
			MaxActiveJourneys int    `json:"max_active_journeys"`
			Digest            string `json:"digest"`
		}
		if err := json.Unmarshal(run.Settings, &recorded); err != nil || recorded.MaxActiveJourneys != 2 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(recorded.Digest) {
			t.Fatalf("settings = %s (%v)", run.Settings, err)
		}
		var info struct{ Name, Version string }
		if err := json.Unmarshal(run.ServerInfo, &info); err != nil || info.Name != "tenancy-mcp" || info.Version != "v9" {
			t.Fatalf("server info = %s (%v)", run.ServerInfo, err)
		}
		if server.Calls("lookup") != 1 {
			t.Fatalf("lookup calls = %d, want 1", server.Calls("lookup"))
		}
	})

	t.Run("unknown tenant", func(t *testing.T) {
		server := newServer(t)
		var models atomic.Int32
		run := submit(t, testTenancy(t, server.URL()), &models, "tenant-unknown")
		wantUnavailable(t, run, &models, server)
		if run.ProductVersion != "" {
			t.Fatalf("scope recorded for an unresolved tenant: %q", run.ProductVersion)
		}
	})

	t.Run("directory names a non-allowlisted endpoint", func(t *testing.T) {
		allowed, ghost := newServer(t), newServer(t)
		allow, err := tenantdir.NewAllowlist(map[string]string{testEndpointID: allowed.URL()})
		if err != nil {
			t.Fatal(err)
		}
		tenancy := testTenancy(t, allowed.URL())
		tenancy.Endpoints = allow
		// "ghost" would resolve to a live server, but is not on the allowlist.
		tenancy.Directory = tenantdir.NewMutable(map[string]tenantdir.Entry{testTenant: {ProductVersion: "vN", EndpointID: "ghost"}})
		var models atomic.Int32
		run := submit(t, tenancy, &models, testTenant)
		wantUnavailable(t, run, &models, allowed, ghost)
	})

	t.Run("settings failure", func(t *testing.T) {
		server := newServer(t)
		source, err := settings.NewMutable(settings.Document{})
		if err != nil {
			t.Fatal(err)
		}
		source.Fail(errors.New("down"))
		tenancy := testTenancy(t, server.URL())
		tenancy.Settings = source
		var models atomic.Int32
		run := submit(t, tenancy, &models, testTenant)
		wantUnavailable(t, run, &models, server)
		if run.ProductVersion != "" || run.EndpointID != "" || len(run.Settings) != 0 {
			t.Fatalf("scope recorded despite settings failure: %+v", run)
		}
	})

	t.Run("allowlisted endpoint refuses connections", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		url := dead.URL
		dead.Close()
		var models atomic.Int32
		run := submit(t, testTenancy(t, url), &models, testTenant)
		wantUnavailable(t, run, &models)
		if run.ProductVersion != "vN" || run.EndpointID != testEndpointID {
			t.Fatalf("scope should be recorded before the connect failure: %+v", run)
		}
	})

	t.Run("chat surfaces RUN_ERROR dependency_unavailable", func(t *testing.T) {
		server := newServer(t)
		var models atomic.Int32
		pool := database(t)
		replica := startAcceptanceReplica(t, pool, acceptanceSet(t, "read_only"), scripted(&models), testTenancy(t, server.URL()))
		// token-b authenticates as tenant-b/bob; tenant-b has no directory entry.
		response := acceptancePostWithHeaders(t, replica.url, aguitypes.RunAgentInput{
			ThreadID: "unrouted", RunID: "unrouted-run",
			Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan"}},
		}, http.Header{"Authorization": {"Bearer token-b"}})
		body := e2eReadBody(t, "unrouted tenant", response)
		if !strings.Contains(body, "RUN_ERROR") || !strings.Contains(body, `"code":"dependency_unavailable"`) || strings.Contains(body, "RUN_FINISHED") {
			t.Fatalf("expected RUN_ERROR dependency_unavailable: %s", body)
		}
		if models.Load() != 0 || len(server.Requests()) != 0 {
			t.Fatalf("model calls = %d, MCP requests = %d, want 0", models.Load(), len(server.Requests()))
		}
		var thread string
		if err := pool.QueryRow(t.Context(), "SELECT id FROM chat_threads WHERE principal='tenant-b/bob' AND external_id='unrouted'").Scan(&thread); err != nil {
			t.Fatal(err)
		}
		snapshot, err := journal.New(pool).Snapshot(t.Context(), thread, "tenant-b/bob")
		if err != nil || len(snapshot.Runs) != 1 || snapshot.Runs[0].State != journal.RunDependencyUnavailable {
			t.Fatalf("durable run = %+v, %v", snapshot.Runs, err)
		}
		// Reconnect projects the same terminal.
		reconnect := e2eReadBody(t, "reconnect", func() *http.Response {
			req, _ := http.NewRequest(http.MethodGet, replica.url+"?threadId=unrouted&runId=unrouted-run", nil)
			req.Header.Set("Authorization", "Bearer token-b")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			return res
		}())
		if !strings.Contains(reconnect, `"code":"dependency_unavailable"`) {
			t.Fatalf("reconnect lost dependency_unavailable: %s", reconnect)
		}
	})
}
