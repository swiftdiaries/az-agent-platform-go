package integration_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/mcptest"
	"github.com/swiftdiaries/az-agent-platform-go/internal/modeltest"
)

// stage gates subtest k on the e2eReachedStage progress ratchet.
func stage(t *testing.T, k int, hop string) {
	t.Helper()
	if k > e2eReachedStage {
		t.Skip("pending: " + hop)
	}
}

// e2eReadBody reads a whole SSE response body and requires status 200.
func e2eReadBody(t *testing.T, name string, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("%s: reading body: %v", name, err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s: status = %d, body: %s", name, response.StatusCode, body)
	}
	return string(body)
}

func TestE2EConversationAgent(t *testing.T) {
	t.Run("S0", func(t *testing.T) {
		stage(t, 0, "H0")
		const (
			thread = "e2e-s0"
			run    = "e2e-s0-run"
			answer = "Kyoto is available for your trip"
		)
		pool := database(t)
		endpoint := mcptest.New(t, mcptest.Options{Name: "e2e", Version: "vN"}, mcptest.Catalog{Tools: []mcptest.Tool{
			{Name: "lookup", OperationKey: "travel.lookup", Result: "Kyoto is available"},
		}})
		registry := acceptanceRegistry(t, endpoint.URL(), "read_only")
		model := modeltest.New(
			modeltest.Call("lookup", map[string]any{"destination": "Kyoto"}),
			modeltest.Text(answer),
		)
		a, b, c := threeReplicas(t, pool, registry, model)

		body := e2eReadBody(t, "POST on A", acceptancePost(t, a.url, aguitypes.RunAgentInput{
			ThreadID: thread, RunID: run,
			Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan a trip to Kyoto"}},
		}))
		if !strings.Contains(body, answer) || !strings.Contains(body, "RUN_FINISHED") {
			t.Fatalf("POST on A lacks final answer or RUN_FINISHED: %s", body)
		}

		for name, replica := range map[string]acceptanceReplica{"B": b, "C": c} {
			replay := e2eReadBody(t, "reconnect on "+name, acceptanceReconnect(t, replica.url, thread, run))
			if !strings.Contains(replay, answer) || !strings.Contains(replay, "RUN_FINISHED") {
				t.Fatalf("reconnect on %s lacks final answer or RUN_FINISHED: %s", name, replay)
			}
		}

		if got := endpoint.Calls("lookup"); got != 1 {
			t.Fatalf("MCP lookup calls = %d, want 1", got)
		}
		if got := endpoint.Applied("lookup"); got != 1 {
			t.Fatalf("MCP lookup applied = %d, want 1", got)
		}
		if got := endpoint.Count("tools/call"); got != 1 {
			t.Fatalf("MCP tools/call count = %d, want 1", got)
		}
		var toolCall *mcptest.Request
		requests := endpoint.Requests()
		for i := range requests {
			if requests[i].RPCMethod == "tools/call" {
				toolCall = &requests[i]
				break
			}
		}
		if toolCall == nil {
			t.Fatal("no tools/call request recorded by the MCP endpoint")
		}
		if toolCall.Tool != "lookup" {
			t.Fatalf("tools/call tool = %q, want lookup", toolCall.Tool)
		}
		if toolCall.Header.Get("Authorization") != "Bearer token" {
			t.Fatal("tools/call did not carry the forwarded Authorization credential")
		}

		if got := model.Count(); got != 2 {
			t.Fatalf("model requests = %d, want 2", got)
		}
		requestsSeen := model.Requests()
		offered := false
		for _, tool := range requestsSeen[0].Tools {
			if tool.Name == "lookup" {
				offered = true
			}
		}
		if !offered {
			t.Fatalf("model request 0 did not offer tool lookup: %#v", requestsSeen[0].Tools)
		}
		// Current code does not populate ToolResult.Name (runtime/run.go:199 sets
		// only CallID and Value), so tie the single result to the lookup call by
		// its CallID, which modeltest derives from the request index. Later hops
		// that populate Name should tighten this to Name == "lookup".
		if results := requestsSeen[1].ToolResults; len(results) != 1 || results[0].CallID != "modeltest-call-0" || results[0].Value == nil {
			t.Fatalf("model request 1 tool results = %#v, want exactly one result for call modeltest-call-0", results)
		}

		threadID := acceptanceThread(t, pool, thread)
		snapshot, err := journal.New(pool).Snapshot(t.Context(), threadID, "alice")
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Runs) != 1 || snapshot.Runs[0].State != journal.RunCompleted {
			t.Fatalf("durable runs = %#v, want one completed run", snapshot.Runs)
		}
	})
	t.Run("S1", func(t *testing.T) {
		stage(t, 1, "H1")
		// A sentinel credential never appears in any DB text/jsonb column, events, slog output or spans, while MCP still receives it.
	})
	t.Run("S2", func(t *testing.T) {
		stage(t, 2, "H2")
		// Tenant A routes to vN and tenant B to vN+1; a non-allowlisted directory endpoint yields dependency_unavailable with zero requests.
	})
	t.Run("S3", func(t *testing.T) {
		stage(t, 3, "H3")
		// A paginated catalog snapshot gives both tenants the same aliases while MCP receives each tenant's own remote names; a second run revalidates without tools/list.
	})
	t.Run("S4", func(t *testing.T) {
		stage(t, 4, "H4b")
		// After activate_journey only that journey's aliases are offered; per-tenant and per-user limits evict LRU journeys, pinned ones stay, and the active set survives restarts.
	})
	t.Run("S5", func(t *testing.T) {
		stage(t, 5, "H4b")
		// A correction admitted via replica B while A's tool is blocked reaches every later provider request, and a reconnect on C is gap-free.
	})
	t.Run("S6", func(t *testing.T) {
		stage(t, 6, "H5")
		// Approval wait, tenant moved to vN+1, approve: superseded plus a new interaction, then dispatched exactly once on vN+1; clarification replies survive the flip.
	})
	t.Run("S7", func(t *testing.T) {
		stage(t, 7, "H6")
		// The real litellm provider against a scripted Responses stub gets attribution headers without credentials, maps 429 budget_exceeded and Admit denials to limit_exceeded, and agent_usage matches reported tokens.
	})
	t.Run("S8", func(t *testing.T) {
		stage(t, 8, "H7")
		// An apply-then-drop-response effect yields outcome_unknown with no retry and a pinned journey, and the trace carries no content, tool descriptions or remote names.
	})
}
