package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/chat"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	platformmcp "github.com/swiftdiaries/az-agent-platform-go/internal/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/platform"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
)

func waitDatabase(t *testing.T, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var ready bool
		if err := pool.QueryRow(ctx, query, args...).Scan(&ready); err != nil {
			t.Fatal(err)
		}
		if ready {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("database barrier timed out")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestAdmissionFinishBothCommitOrders(t *testing.T) {
	for _, state := range []journal.RunState{journal.RunCompleted, journal.RunFailed, journal.RunAuthRequired} {
		for _, first := range []string{"admission", "finish"} {
			t.Run(string(state)+"/"+first, func(t *testing.T) {
				ctx := context.Background()
				pool := database(t)
				store := journal.New(pool)
				c := journal.Command{ThreadID: "thread", RunID: "run", CommunicationID: "first", Principal: "alice", Text: "first"}
				if _, _, err := store.Admit(ctx, c); err != nil {
					t.Fatal(err)
				}
				owner := claimForTest(t, store, c, true)
				if _, err := store.Start(ctx, owner, "planner", strings.Repeat("a", 64)); err != nil {
					t.Fatal(err)
				}
				steering := c
				steering.RunID = "steering"
				steering.CommunicationID = "second"
				steering.Text = "correction"
				gate, err := pool.Acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer gate.Release()
				if _, err := gate.Exec(ctx, "SELECT pg_advisory_lock(345678)"); err != nil {
					t.Fatal(err)
				}
				trigger := `CREATE FUNCTION admission_finish_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(345678); RETURN NEW; END $$; `
				if first == "admission" {
					trigger += `CREATE TRIGGER admission_finish_barrier BEFORE INSERT ON agent_commands FOR EACH ROW EXECUTE FUNCTION admission_finish_barrier()`
				} else {
					trigger += `CREATE TRIGGER admission_finish_barrier BEFORE UPDATE ON agent_runs FOR EACH ROW WHEN (NEW.state IN ('completed','failed','auth_required')) EXECUTE FUNCTION admission_finish_barrier()`
				}
				if _, err := pool.Exec(ctx, trigger); err != nil {
					t.Fatal(err)
				}
				type admission struct {
					receipt journal.Receipt
					fresh   bool
					err     error
				}
				admitted := make(chan admission, 1)
				finished := make(chan error, 1)
				admit := func() { r, f, e := store.Admit(ctx, steering); admitted <- admission{r, f, e} }
				finish := func() { finished <- store.Finish(ctx, owner, state, []byte(`[]`), "answer", "", "") }
				if first == "admission" {
					go admit()
				} else {
					go finish()
				}
				waitDatabase(t, pool, "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE NOT granted AND locktype='advisory')")
				if first == "admission" {
					go finish()
				} else {
					go admit()
				}
				waitDatabase(t, pool, "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE NOT granted AND locktype='transactionid')")
				if _, err := gate.Exec(ctx, "SELECT pg_advisory_unlock(345678)"); err != nil {
					t.Fatal(err)
				}
				a, f := <-admitted, <-finished
				if a.err != nil {
					t.Fatal(a.err)
				}
				if first == "admission" && state == journal.RunCompleted {
					if !errors.Is(f, journal.ErrPending) || a.fresh || a.receipt.ExecutionRunID != c.RunID {
						t.Fatalf("admission-first: %+v finish %v", a, f)
					}
					inbox, err := store.Pending(ctx, owner)
					if err != nil || len(inbox.Commands) != 1 || inbox.Commands[0].Text != "correction" {
						t.Fatalf("orphaned command: %+v %v", inbox, err)
					}
					if err := store.Included(ctx, owner, inbox.Commands); err != nil {
						t.Fatal(err)
					}
					if err := store.Finish(ctx, owner, journal.RunCompleted, []byte(`[]`), "corrected", "", ""); err != nil {
						t.Fatal(err)
					}
				} else if first == "admission" {
					if f != nil || a.fresh || a.receipt.ExecutionRunID != c.RunID {
						t.Fatalf("admission-before-failure: %+v finish %v", a, f)
					}
					snapshot, err := store.Snapshot(ctx, c.ThreadID, c.Principal)
					if err != nil {
						t.Fatal(err)
					}
					if snapshot.Runs[0].PendingCommands != 0 {
						t.Fatalf("terminal %s stranded accepted commands: %+v", state, snapshot)
					}
					rejected := false
					for _, event := range snapshot.Events {
						if event.Type == "command.rejected" && event.CommunicationID == steering.CommunicationID && event.Reason == string(state) {
							rejected = true
						}
					}
					if !rejected {
						t.Fatal("accepted steering has no terminal disposition")
					}
				} else if f != nil || !a.fresh || a.receipt.ExecutionRunID != steering.RunID {
					t.Fatalf("finish-first: %+v finish %v", a, f)
				}
				beforeRetry, err := store.Snapshot(ctx, c.ThreadID, c.Principal)
				if err != nil {
					t.Fatal(err)
				}
				assertCommandOutcome(t, beforeRetry, c.CommunicationID, "included", "")
				disposition, reason := "included", ""
				if first == "finish" {
					disposition = "pending"
				} else if state != journal.RunCompleted {
					disposition = "rejected"
					reason = string(state)
				}
				assertCommandOutcome(t, beforeRetry, steering.CommunicationID, disposition, reason)
				duplicate, fresh, err := store.Admit(ctx, steering)
				if err != nil || fresh || !reflect.DeepEqual(duplicate, a.receipt) {
					t.Fatalf("unstable receipt %+v %v %v", duplicate, fresh, err)
				}
				steering.Text = "changed"
				if _, _, err := store.Admit(ctx, steering); !errors.Is(err, journal.ErrConflict) {
					t.Fatalf("changed duplicate: %v", err)
				}
				afterRetry, err := store.Snapshot(ctx, c.ThreadID, c.Principal)
				if err != nil || !reflect.DeepEqual(beforeRetry, afterRetry) {
					t.Fatalf("duplicate changed terminal disposition: %+v %v", afterRetry, err)
				}
				var live int
				if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_runs WHERE state IN ('pending','running')").Scan(&live); err != nil {
					t.Fatal(err)
				}
				want := 0
				if first == "finish" {
					want = 1
				}
				if live != want {
					t.Fatalf("live runs %d want %d", live, want)
				}
			})
		}
	}
}

func TestSteeringBlockedToolAcrossThreeReplicas(t *testing.T) {
	ctx := context.Background()
	pool := database(t)
	started, release := make(chan struct{}), make(chan struct{})
	server := mcp.NewServer(&mcp.Implementation{Name: "steering", Version: "1"}, nil)
	var toolCalls atomic.Int64
	mcp.AddTool(server, &mcp.Tool{Name: "lookup_destination", Description: "read options"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		toolCalls.Add(1)
		close(started)
		select {
		case <-release:
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "available Kyoto"}}}, struct{}{}, nil
		case <-ctx.Done():
			return nil, struct{}{}, ctx.Err()
		}
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	var wrongCredentials atomic.Bool
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "owner-secret" {
			wrongCredentials.Store(true)
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	t.Setenv("TEST_JAVA_MCP_ENDPOINT", endpoint.URL)
	registry, err := definitions.Load(filepath.Join("..", "configs", "journeys.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var modelCalls atomic.Int64
	observed := make(chan agentruntime.ModelRequest, 4)
	model := modelFunc(func(_ context.Context, request agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		modelCalls.Add(1)
		observed <- request
		if len(request.ToolResults) == 0 {
			return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{Name: "lookup_destination", Arguments: json.RawMessage(`{}`)}}, nil
		}
		return agentruntime.ModelResponse{Text: "corrected Kyoto"}, nil
	})
	runner := agentruntime.NewRunner(registry, platformmcp.NewClient(), model)
	auth := chat.StaticBearerTokens{"token": "alice"}
	var apis []*httptest.Server
	for range 3 {
		service := platform.New(runner, journal.New(pool))
		t.Cleanup(service.Close)
		api := httptest.NewServer(chat.NewHandler(auth, service, pool))
		defer api.Close()
		apis = append(apis, api)
	}
	post := func(replica int, run, text, cookie string) *http.Response {
		t.Helper()
		payload, _ := json.Marshal(aguitypes.RunAgentInput{ThreadID: "external-thread", RunID: run, Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: text}}})
		req, _ := http.NewRequest(http.MethodPost, apis[replica].URL, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer token")
		req.Header.Set("Cookie", cookie)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 {
			t.Fatalf("POST status %d", response.StatusCode)
		}
		return response
	}
	a := post(0, "run-a", "initial request", "owner-secret")
	defer a.Body.Close()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not start")
	}
	b := post(1, "run-b", "first correction", "replica-b-secret")
	defer b.Body.Close()
	b2 := post(1, "run-b2", "second correction", "replica-b-secret")
	defer b2.Body.Close()
	req, _ := http.NewRequest(http.MethodGet, apis[2].URL+"?threadId=external-thread&runId=run-b", nil)
	req.Header.Set("Authorization", "Bearer token")
	c, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Body.Close()
	var thread string
	if err := pool.QueryRow(ctx, "SELECT id FROM agent_conversations").Scan(&thread); err != nil {
		t.Fatal(err)
	}
	before, err := journal.New(pool).Snapshot(ctx, thread, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Runs) != 1 || before.Runs[0].PendingCommands != 2 {
		t.Fatalf("steering started another execution: %+v", before)
	}
	close(release)
	for label, response := range map[string]*http.Response{"A": a, "B": b, "B2": b2, "C": c} {
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		envelope := map[string]string{"A": "run-a", "B": "run-b", "B2": "run-b2", "C": "run-b"}[label]
		assertEdgeCommandOutcomes(t, data, envelope, map[string]string{"run-b": "included", "run-b2": "included"}, before.Runs[0].CommandOutcomes)
		if label == "C" {
			var ids []int64
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "id: ") {
					id, err := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
					if err != nil {
						t.Fatal(err)
					}
					ids = append(ids, id)
				}
			}
			if len(ids) < 5 {
				t.Fatalf("C missed journal events: %v", ids)
			}
			for i, id := range ids {
				if id != before.Watermark+int64(i) {
					t.Fatalf("C missing/duplicate sequence at %d: %v", i, ids)
				}
			}
		}
		if !bytes.Contains(data, []byte("corrected Kyoto")) || !bytes.Contains(data, []byte("RUN_FINISHED")) {
			t.Fatalf("replica %s replay: %s", label, data)
		}
	}
	first, second := <-observed, <-observed
	if first.Handoff.JourneyID != "vacation-planner" || len(first.PendingCommands) != 1 || len(first.History) == 0 {
		t.Fatalf("missing explicit provider input: %+v", first)
	}
	if len(second.PendingCommands) != 2 || second.PendingCommands[0].Text != "first correction" || second.PendingCommands[1].Text != "second correction" || len(second.ToolResults) != 1 || !bytes.Contains(second.History, []byte("available Kyoto")) {
		t.Fatalf("wrong actual next provider request: %+v", second)
	}
	if modelCalls.Load() != 2 || toolCalls.Load() != 1 || wrongCredentials.Load() {
		t.Fatalf("replay or credential transfer: models %d tools %d credentials %v", modelCalls.Load(), toolCalls.Load(), wrongCredentials.Load())
	}
	after, err := journal.New(pool).Snapshot(ctx, thread, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if after.RunState != journal.RunCompleted || after.Runs[0].PendingCommands != 0 {
		t.Fatalf("orphaned commands: %+v", after)
	}
	included := 0
	for i, event := range after.Events {
		if event.Sequence != int64(i+1) {
			t.Fatalf("sequence gap: %+v", event)
		}
		if event.Type == "command.included" {
			included++
		}
	}
	if included != 3 {
		t.Fatalf("included %d commands", included)
	}
	freshService := platform.New(runner, journal.New(pool))
	defer freshService.Close()
	freshAPI := httptest.NewServer(chat.NewHandler(auth, freshService, pool))
	defer freshAPI.Close()
	for _, run := range []string{"run-b", "run-b2"} {
		data := reconnectChat(t, freshAPI.URL, "external-thread", run, "token")
		assertEdgeCommandOutcomes(t, data, run, map[string]string{"run-b": "included", "run-b2": "included"}, after.Runs[0].CommandOutcomes)
	}
	if modelCalls.Load() != 2 || toolCalls.Load() != 1 {
		t.Fatal("fresh Chat reconnect reexecuted the run")
	}
	var durable string
	if err := pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT jsonb_agg(c) FROM agent_commands c),(SELECT jsonb_agg(s) FROM agent_sessions s),(SELECT jsonb_agg(r) FROM agent_runs r),(SELECT jsonb_agg(e) FROM agent_events e))::text`).Scan(&durable); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(durable, "secret") || strings.Contains(durable, "Bearer") {
		t.Fatal("credentials persisted")
	}
}

func TestSteeringInclusionFailureStopsDispatch(t *testing.T) {
	ctx := context.Background()
	pool := database(t)
	if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_inclusion() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected inclusion failure'; END $$; CREATE TRIGGER reject_inclusion BEFORE UPDATE OF included ON agent_commands FOR EACH ROW EXECUTE FUNCTION reject_inclusion()`); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	runner := fixtureRunner(t, modelFunc(func(_ context.Context, request agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		calls.Add(1)
		if len(request.PendingCommands) != 1 {
			return agentruntime.ModelResponse{}, errors.New("missing boundary command")
		}
		return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{Name: "lookup_destination", Arguments: json.RawMessage(`{}`)}}, nil
	}))
	service := platform.New(runner, journal.New(pool))
	defer service.Close()
	api := httptest.NewServer(chat.NewHandler(chat.StaticBearerTokens{"token": "alice"}, service, pool))
	defer api.Close()
	body := aguitypes.RunAgentInput{ThreadID: "external", RunID: "run", Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "hello"}}}
	response := postAGUI(t, api.URL, body, "token", "", "")
	if response.StatusCode != 200 {
		t.Fatalf("submission: %+v", response)
	}
	var thread string
	if err := pool.QueryRow(ctx, "SELECT id FROM agent_conversations").Scan(&thread); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Snapshot(ctx, thread, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || snapshot.Runs[0].PendingCommands != 0 {
		t.Fatalf("failure did not stop: calls %d snapshot %+v", calls.Load(), snapshot)
	}
	for _, event := range snapshot.Events {
		if event.Type == "command.included" || event.Type == "tool.completed" || event.Type == "message.completed" {
			t.Fatalf("false success: %+v", event)
		}
	}
	assertCommandOutcome(t, snapshot, snapshot.Runs[0].CommandOutcomes[0].CommunicationID, "rejected", "failed")
	// Reconnect above the watermark must still show the rejected command and reason.
	req, _ := http.NewRequest(http.MethodGet, api.URL+"?threadId=external&runId=run", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Last-Event-ID", "999")
	reconnected, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Body.Close()
	data, err := io.ReadAll(reconnected.Body)
	if err != nil || !bytes.Contains(data, []byte(`"state":"rejected"`)) || !bytes.Contains(data, []byte(`"reason":"failed"`)) {
		t.Fatalf("missing snapshot disposition: %s %v", data, err)
	}
	retry := postAGUI(t, api.URL, body, "token", "", "")
	if retry.StatusCode != 200 || calls.Load() != 1 {
		t.Fatalf("failed command retried provider: %+v calls %d", retry, calls.Load())
	}
	var history string
	if err := pool.QueryRow(ctx, "SELECT history::text FROM agent_sessions").Scan(&history); err != nil || history != "[]" {
		t.Fatalf("failed request history committed %s: %v", history, err)
	}
}

func TestSteeringFinalDrainUsesLiveOwner(t *testing.T) {
	ctx := context.Background()
	pool := database(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	runner := fixtureRunner(t, modelFunc(func(_ context.Context, request agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return agentruntime.ModelResponse{Text: "first answer"}, nil
		}
		if len(request.PendingCommands) != 1 || request.PendingCommands[0].Text != "late correction" || !bytes.Contains(request.History, []byte("first answer")) {
			return agentruntime.ModelResponse{}, errors.New("final drain lost history or input")
		}
		return agentruntime.ModelResponse{Text: "final corrected answer"}, nil
	}))
	a := platform.New(runner, journal.New(pool))
	defer a.Close()
	b := platform.New(runner, journal.New(pool))
	defer b.Close()
	c := platform.Command{ThreadID: "thread", RunID: "run", CommunicationID: "initial", Principal: "alice", Text: "initial"}
	if _, err := a.Submit(ctx, platform.Submission{Command: c}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}
	c.RunID = "late"
	c.CommunicationID = "late"
	c.Text = "late correction"
	if _, err := b.Submit(ctx, platform.Submission{Command: c}); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitDatabase(t, pool, "SELECT EXISTS(SELECT 1 FROM agent_runs WHERE state='completed')")
	snapshot, err := b.Snapshot(ctx, "thread", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || len(snapshot.Runs) != 1 || snapshot.Runs[0].PendingCommands != 0 || snapshot.Answer != "final corrected answer" {
		t.Fatalf("stranded final drain: %+v calls %d", snapshot, calls.Load())
	}
}

func TestOwnershipRenewalFailureCancelsLocalTool(t *testing.T) {
	ctx := context.Background()
	pool := database(t)
	started, release, remoteFinished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var toolCalls, modelCalls atomic.Int64
	server := mcp.NewServer(&mcp.Implementation{Name: "ownership-loss", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "lookup_destination", Description: "read lookup"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		toolCalls.Add(1)
		close(started)
		<-release // A remote server may finish even after local HTTP cancellation.
		close(remoteFinished)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "late result"}}}, struct{}{}, nil
	})
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer endpoint.Close()
	t.Setenv("TEST_JAVA_MCP_ENDPOINT", endpoint.URL)
	registry, err := definitions.Load(filepath.Join("..", "configs", "journeys.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	contexts := make(chan context.Context, 1)
	model := modelFunc(func(ctx context.Context, _ agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		modelCalls.Add(1)
		contexts <- ctx
		return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{Name: "lookup_destination", Arguments: json.RawMessage(`{}`)}}, nil
	})
	runner := agentruntime.NewRunner(registry, platformmcp.NewClient(), model)
	a := platform.New(runner, journal.New(pool))
	defer a.Close()
	b := platform.New(runner, journal.New(pool))
	defer b.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	c := platform.Command{ThreadID: "thread", RunID: "run", CommunicationID: "initial", Principal: "alice", Text: "initial"}
	if _, err := a.Submit(ctx, platform.Submission{Command: c}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not start")
	}
	localContext := <-contexts
	c.RunID = "pending"
	c.CommunicationID = "pending"
	c.Text = "pending correction"
	if _, err := b.Submit(ctx, platform.Submission{Command: c}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_renewal() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'database owner renewal unavailable'; END $$; CREATE TRIGGER reject_renewal BEFORE UPDATE OF lease_until ON agent_runs FOR EACH ROW EXECUTE FUNCTION reject_renewal()`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-localContext.Done():
	case <-time.After(8 * time.Second):
		t.Fatal("renewal failure did not cancel local execution")
	}
	closed := make(chan struct{})
	go func() { a.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("local cleanup waited for remote tool completion")
	}
	select {
	case <-remoteFinished:
		t.Fatal("test remote work canceled or completed before release")
	default:
	}
	if _, err := pool.Exec(ctx, "DROP TRIGGER reject_renewal ON agent_runs; UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if err := journal.New(pool).Reap(ctx); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-remoteFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("remote work did not finish")
	}
	snapshot, err := b.Snapshot(ctx, "thread", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RunState != journal.RunInterrupted || snapshot.Runs[0].PendingCommands != 0 || toolCalls.Load() != 1 || modelCalls.Load() != 1 {
		t.Fatalf("takeover/late tool result: %+v calls %d/%d", snapshot, toolCalls.Load(), modelCalls.Load())
	}
	assertCommandOutcome(t, snapshot, "initial", "included", "")
	assertCommandOutcome(t, snapshot, "pending", "interrupted", "interrupted")
	for _, event := range snapshot.Events {
		if event.Type == "tool.completed" || event.Type == "message.completed" {
			t.Fatalf("late result committed: %+v", event)
		}
	}
}

func assertCommandOutcome(t *testing.T, snapshot journal.Snapshot, communication, state, reason string) {
	t.Helper()
	for _, run := range snapshot.Runs {
		for _, outcome := range run.CommandOutcomes {
			if outcome.CommunicationID != communication {
				continue
			}
			if outcome.State != state || outcome.Reason != reason {
				t.Fatalf("command disposition: %+v want %s/%s", outcome, state, reason)
			}
			if state == "rejected" || state == "interrupted" {
				found := 0
				for _, event := range snapshot.Events {
					if event.CommunicationID == communication && event.Type == "command."+state && event.Reason == reason {
						found++
					}
				}
				if found != 1 {
					t.Fatalf("command %s has %d terminal disposition events", communication, found)
				}
			}
			return
		}
	}
	t.Fatalf("no outcome for accepted command %s", communication)
}

func TestAdmissionFinishDispositionAtomic(t *testing.T) {
	for _, state := range []journal.RunState{journal.RunFailed, journal.RunAuthRequired, journal.RunInterrupted} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			pool := database(t)
			store := journal.New(pool)
			c := journal.Command{ThreadID: "thread", RunID: "run", CommunicationID: "comm", Principal: "alice", Text: "hello"}
			if _, _, err := store.Admit(ctx, c); err != nil {
				t.Fatal(err)
			}
			owner := claimForTest(t, store, c, false)
			if state == journal.RunInterrupted {
				if _, err := pool.Exec(ctx, "UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second'"); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.Snapshot(ctx, c.ThreadID, c.Principal)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_terminal_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected terminal event failure'; END $$; CREATE TRIGGER reject_terminal_event BEFORE INSERT ON agent_events FOR EACH ROW WHEN (NEW.kind IN ('run.failed','auth_required','run.interrupted')) EXECUTE FUNCTION reject_terminal_event()`); err != nil {
				t.Fatal(err)
			}
			finish := func() error {
				if state == journal.RunInterrupted {
					return store.Reap(ctx)
				}
				return store.Finish(ctx, owner, state, nil, "", "", "")
			}
			if err := finish(); err == nil {
				t.Fatal("terminal event failure did not roll back")
			}
			after, err := store.Snapshot(ctx, c.ThreadID, c.Principal)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("partial command disposition: %+v %v", after, err)
			}
			if _, err := pool.Exec(ctx, "DROP TRIGGER reject_terminal_event ON agent_events"); err != nil {
				t.Fatal(err)
			}
			if err := finish(); err != nil {
				t.Fatal(err)
			}
			after, err = store.Snapshot(ctx, c.ThreadID, c.Principal)
			if err != nil {
				t.Fatal(err)
			}
			disposition := "rejected"
			if state == journal.RunInterrupted {
				disposition = "interrupted"
			}
			assertCommandOutcome(t, after, c.CommunicationID, disposition, string(state))
			if after.Runs[0].PendingCommands != 0 {
				t.Fatal("terminal command still pending")
			}
		})
	}
}

// Command correlation is independent of the enclosing observed execution stream.
func assertEdgeCommandOutcomes(t *testing.T, data []byte, envelope string, want map[string]string, internal []journal.CommandOutcome) {
	t.Helper()
	for _, command := range internal {
		if bytes.Contains(data, []byte(command.CommunicationID)) {
			t.Fatalf("internal command identity exposed at Chat edge: %s", data)
		}
	}
	found := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var frame struct {
			Type, RunID, Name string
			Snapshot          struct {
				Commands []struct{ RunID, State string }
			}
			Value struct{ RunID string }
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatal(err)
		}
		if frame.RunID != "" && frame.RunID != envelope {
			t.Fatalf("execution envelope %s was replaced with command ID %s", envelope, frame.RunID)
		}
		for _, command := range frame.Snapshot.Commands {
			if command.RunID == "" {
				t.Fatal("snapshot command has no external run ID")
			}
			found[command.RunID] = command.State
		}
		if strings.HasPrefix(frame.Name, "command.") {
			if frame.Value.RunID == "" {
				t.Fatal("live command outcome has no external run ID")
			}
			state := strings.TrimPrefix(frame.Name, "command.")
			if state == "accepted" {
				state = "pending"
			}
			found[frame.Value.RunID] = state
		}
	}
	for run, state := range want {
		if found[run] != state {
			t.Fatalf("external command %s outcome %q want %q; all outcomes %v", run, found[run], state, found)
		}
	}
}

func reconnectChat(t *testing.T, endpoint, thread, run, token string) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, endpoint+"?threadId="+thread+"&runId="+run, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Last-Event-ID", "999")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("reconnect status %d: %s %v", response.StatusCode, data, err)
	}
	return data
}

func TestSteeringTerminalCommandCorrelationAcrossReplicas(t *testing.T) {
	for _, state := range []journal.RunState{journal.RunFailed, journal.RunInterrupted} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			pool := database(t)
			started, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int64
			runner := fixtureRunner(t, modelFunc(func(_ context.Context, _ agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
				calls.Add(1)
				close(started)
				<-release
				return agentruntime.ModelResponse{}, errors.New("model failed")
			}))
			auth := chat.StaticBearerTokens{"token": "alice", "other": "bob"}
			owner := platform.New(runner, journal.New(pool))
			defer owner.Close()
			peer := platform.New(runner, journal.New(pool))
			defer peer.Close()
			a := httptest.NewServer(chat.NewHandler(auth, owner, pool))
			defer a.Close()
			b := httptest.NewServer(chat.NewHandler(auth, peer, pool))
			defer b.Close()
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			post := func(endpoint, run, text string) *http.Response {
				payload, _ := json.Marshal(aguitypes.RunAgentInput{ThreadID: "external-thread", RunID: run, Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: text}}})
				req, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
				req.Header.Set("Authorization", "Bearer token")
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != 200 {
					t.Fatalf("POST status %d", response.StatusCode)
				}
				return response
			}
			first := post(a.URL, "owner-run", "hold")
			defer first.Body.Close()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("model did not start")
			}
			correction1 := post(b.URL, "correction-one", "one")
			defer correction1.Body.Close()
			correction2 := post(b.URL, "correction-two", "two")
			defer correction2.Body.Close()
			var thread string
			if err := pool.QueryRow(ctx, "SELECT id FROM agent_conversations").Scan(&thread); err != nil {
				t.Fatal(err)
			}
			snapshot, err := journal.New(pool).Snapshot(ctx, thread, "alice")
			if err != nil {
				t.Fatal(err)
			}
			disposition := "rejected"
			if state == journal.RunInterrupted {
				disposition = "interrupted"
				if _, err := pool.Exec(ctx, "UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second'"); err != nil {
					t.Fatal(err)
				}
				if err := journal.New(pool).Reap(ctx); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			want := map[string]string{"correction-one": disposition, "correction-two": disposition}
			for run, response := range map[string]*http.Response{"owner-run": first, "correction-one": correction1, "correction-two": correction2} {
				data, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				assertEdgeCommandOutcomes(t, data, run, want, snapshot.Runs[0].CommandOutcomes)
			}
			// New Platform and Chat instances restore only durable identities and outcomes.
			fresh := platform.New(runner, journal.New(pool))
			defer fresh.Close()
			c := httptest.NewServer(chat.NewHandler(auth, fresh, pool))
			defer c.Close()
			for _, run := range []string{"correction-one", "correction-two"} {
				data := reconnectChat(t, c.URL, "external-thread", run, "token")
				assertEdgeCommandOutcomes(t, data, run, want, snapshot.Runs[0].CommandOutcomes)
			}
			denied, _ := http.NewRequest(http.MethodGet, c.URL+"?threadId=external-thread&runId=correction-one", nil)
			denied.Header.Set("Authorization", "Bearer other")
			response, err := http.DefaultClient.Do(denied)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusForbidden {
				t.Fatalf("principal crossed command identity boundary: %d", response.StatusCode)
			}
			if calls.Load() != 1 {
				t.Fatalf("outcome observation retried execution: %d", calls.Load())
			}
		})
	}
}
