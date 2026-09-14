package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/chat"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	platformmcp "github.com/swiftdiaries/az-agent-platform-go/internal/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/platform"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
)

func fixtureRunner(t *testing.T, model agentruntime.Model) *agentruntime.Runner {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "persistent-fixture", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "lookup_destination"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		return nil, struct{}{}, nil
	})
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(endpoint.Close)
	t.Setenv("TEST_JAVA_MCP_ENDPOINT", endpoint.URL)
	registry, err := definitions.Load(filepath.Join("..", "configs", "journeys.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return agentruntime.NewRunner(registry, platformmcp.NewClient(), model)
}

// A request-scoped run context would cancel the model here. A process-local ID map
// or missing provider history would lose the remembered answer after replacement.
func TestPersistenceDisconnectAndReplacement(t *testing.T) {
	pool := database(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	model := modelFunc(func(ctx context.Context, r agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		calls.Add(1)
		if r.Messages[len(r.Messages)-1] == "remember Kyoto" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return agentruntime.ModelResponse{}, ctx.Err()
			}
			return agentruntime.ModelResponse{Text: "remembered Kyoto"}, nil
		}
		if !strings.Contains(strings.Join(r.Messages, "|"), "remembered Kyoto") {
			return agentruntime.ModelResponse{}, fmt.Errorf("prior provider history missing")
		}
		return agentruntime.ModelResponse{Text: "Kyoto is remembered"}, nil
	})
	runner := fixtureRunner(t, model)
	service := platform.New(runner, journal.New(pool))
	t.Cleanup(service.Close)
	auth := chat.StaticBearerTokens{"token": "alice", "other": "bob"}
	api := httptest.NewServer(chat.NewHandler(auth, service, pool))
	defer api.Close()
	body := aguitypes.RunAgentInput{ThreadID: "external-email@example.test", RunID: "external-1", Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "remember Kyoto"}}}
	payload, _ := json.Marshal(body)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, api.URL, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model never started")
	}
	cancel()
	response.Body.Close()
	close(release)
	var thread string
	if err := pool.QueryRow(context.Background(), "SELECT id FROM chat_threads WHERE principal='alice'").Scan(&thread); err != nil {
		t.Fatal(err)
	}
	waitCompleted(t, service, thread, "alice")
	service.Close()
	api.Close()
	replacement := platform.New(fixtureRunner(t, model), journal.New(pool))
	t.Cleanup(replacement.Close)
	api2 := httptest.NewServer(chat.NewHandler(auth, replacement, pool))
	defer api2.Close()
	retry := postAGUI(t, api2.URL, body, "token", "fresh-secret", "")
	if !strings.Contains(retry.Body, "remembered Kyoto") || calls.Load() != 1 {
		t.Fatalf("retry repeated execution: %d %s", calls.Load(), retry.Body)
	}
	body.RunID = "external-2"
	body.Messages = []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "what do you remember?"}}
	resumed := postAGUI(t, api2.URL, body, "token", "replacement-secret", "")
	if !strings.Contains(resumed.Body, "Kyoto is remembered") {
		t.Fatalf("idle resume lost history: %s", resumed.Body)
	}
	if calls.Load() != 2 {
		t.Fatalf("model calls %d", calls.Load())
	}
	// A different principal cannot observe Alice's external conversation.
	get, _ := http.NewRequest(http.MethodGet, api2.URL+"?threadId="+body.ThreadID+"&runId=external-2", nil)
	get.Header.Set("Authorization", "Bearer other")
	denied, err := http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	defer denied.Body.Close()
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("observation authorization %d", denied.StatusCode)
	}
	var persisted string
	err = pool.QueryRow(context.Background(), `SELECT (SELECT json_agg(c)::text FROM agent_commands c)||(SELECT json_agg(s)::text FROM agent_sessions s)||(SELECT json_agg(e)::text FROM agent_events e)`).Scan(&persisted)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fresh-secret", "replacement-secret", "Bearer token", body.ThreadID} {
		if strings.Contains(persisted, secret) {
			t.Fatalf("Agent persistence leaked %q", secret)
		}
	}
}
func waitCompleted(t *testing.T, p *platform.Platform, thread, principal string) platform.Snapshot {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		snapshot, err := p.Snapshot(context.Background(), thread, principal)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.RunState == platform.RunCompleted {
			return snapshot
		}
		if snapshot.RunState == platform.RunFailed {
			t.Fatalf("run failed: %+v", snapshot)
		}
		select {
		case <-deadline:
			t.Fatal("completion deadline")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestReplayCursorAndPolling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := database(t)
	store := journal.New(pool)
	c := journal.Command{ThreadID: "thread", Principal: "alice", RunID: "one", CommunicationID: "one", Text: "hi"}
	if _, _, err := store.Admit(ctx, c); err != nil {
		t.Fatal(err)
	}
	p := platform.New(nil, store)
	defer p.Close()
	for _, cursor := range []int64{0, 1, 9} {
		t.Run(fmt.Sprint(cursor), func(t *testing.T) {
			observation, err := p.Observe(ctx, "thread", "alice", cursor)
			if err != nil {
				t.Fatal(err)
			}
			defer observation.Close()
			if observation.Snapshot.Watermark != 1 {
				t.Fatalf("watermark %d", observation.Snapshot.Watermark)
			}
			// Polling works without publishing any notification. Events <= max(cursor,watermark) never reappear.
			if cursor == 9 {
				select {
				case event := <-observation.Events:
					t.Fatalf("event below cursor %+v", event)
				case <-time.After(60 * time.Millisecond):
				}
				return
			}
			// Each observer waits while no post-watermark event exists; this is a journal stream, not a replay of snapshot events.
			select {
			case event := <-observation.Events:
				t.Fatalf("snapshot event repeated %+v", event)
			case <-time.After(60 * time.Millisecond):
			}
		})
	}
	obs, err := p.Observe(ctx, "thread", "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer obs.Close()
	if _, err := store.Start(ctx, c, "planner", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, c, journal.RunCompleted, []byte(`[]`), "done", "", ""); err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for len(seen) < 3 {
		select {
		case e := <-obs.Events:
			if e.Sequence <= 1 || seen[e.Sequence] {
				t.Fatalf("duplicate/below watermark %+v", e)
			}
			seen[e.Sequence] = true
		case <-time.After(3 * time.Second):
			t.Fatal("lost-notification polling failed")
		}
	}
	if _, err := p.Observe(ctx, "thread", "bob", 0); err != platform.ErrForbidden {
		t.Fatalf("observation authorization %v", err)
	}
}

// Hold the journal relation so Snapshot has read its watermark and run records,
// then commit a writer before its event query can finish. ReadCommitted would
// return a torn graph (old watermark with new events) and fail this check.
func TestReplaySnapshotConcurrentCommit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool := database(t)
	store := journal.New(pool)
	c := journal.Command{ThreadID: "thread", RunID: "run", CommunicationID: "comm", Principal: "alice", Text: "hi"}
	if _, _, err := store.Admit(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(ctx, c, "planner", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())
	if _, err = writer.Exec(ctx, "LOCK TABLE agent_events IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	snapshots := make(chan journal.Snapshot, 1)
	failures := make(chan error, 1)
	go func() {
		snapshot, err := store.Snapshot(ctx, "thread", "alice")
		if err != nil {
			failures <- err
			return
		}
		snapshots <- snapshot
	}()
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE relation='agent_events'::regclass AND NOT granted)").Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("snapshot never reached journal read")
		case <-time.After(time.Millisecond):
		}
	}
	if _, err := writer.Exec(ctx, `UPDATE agent_runs SET state='completed',answer='committed later' WHERE id='run';
  UPDATE agent_conversations SET sequence=4 WHERE id='thread';
  INSERT INTO agent_events(thread_id,sequence,run_id,kind,answer) VALUES('thread',3,'run','message.completed','committed later'),('thread',4,'run','run.completed','')`); err != nil {
		t.Fatal(err)
	}
	if err = writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failures:
		t.Fatal(err)
	case snapshot := <-snapshots:
		if snapshot.Watermark != 2 || len(snapshot.Events) != 2 || snapshot.RunState != journal.RunRunning || snapshot.Answer != "" {
			t.Fatalf("torn snapshot: %+v", snapshot)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	after, err := store.Snapshot(ctx, "thread", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if after.Watermark != 4 || len(after.Events) != 4 || after.RunState != journal.RunCompleted {
		t.Fatalf("committed snapshot %+v", after)
	}
}

func TestReplayHTTPReconnectCursor(t *testing.T) {
	pool := database(t)
	var calls atomic.Int64
	runner := fixtureRunner(t, modelFunc(func(context.Context, agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		calls.Add(1)
		return agentruntime.ModelResponse{Text: "durable answer"}, nil
	}))
	p := platform.New(runner, journal.New(pool))
	defer p.Close()
	api := httptest.NewServer(chat.NewHandler(chat.StaticBearerTokens{"token": "alice"}, p, pool))
	defer api.Close()
	input := aguitypes.RunAgentInput{ThreadID: "external", RunID: "run", Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "hi"}}}
	first := postAGUI(t, api.URL, input, "token", "", "")
	if !strings.Contains(first.Body, "durable answer") {
		t.Fatal(first.Body)
	}
	var thread string
	if err := pool.QueryRow(context.Background(), "SELECT id FROM chat_threads").Scan(&thread); err != nil {
		t.Fatal(err)
	}
	snapshot := waitCompleted(t, p, thread, "alice")
	for _, cursor := range []int64{0, snapshot.Watermark, snapshot.Watermark + 20} {
		request, _ := http.NewRequest(http.MethodGet, api.URL+"?threadId=external&runId=run", nil)
		request.Header.Set("Authorization", "Bearer token")
		request.Header.Set("Last-Event-ID", fmt.Sprint(cursor))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 || !bytes.Contains(data, []byte("STATE_SNAPSHOT")) || !bytes.Contains(data, []byte("durable answer")) {
			t.Fatalf("reconnect %d: %s", cursor, data)
		}
		if !bytes.Contains(data, []byte(fmt.Sprintf("id: %d\n", max(cursor, snapshot.Watermark)))) {
			t.Fatalf("cursor regressed: %s", data)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("GET observation executed model %d times", calls.Load())
	}
	request, _ := http.NewRequest(http.MethodGet, api.URL+"?threadId=external&runId=run", nil)
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Last-Event-ID", "-1")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatalf("invalid cursor %d", response.StatusCode)
	}
}
