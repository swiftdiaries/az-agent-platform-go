package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	platformmcp "github.com/swiftdiaries/az-agent-platform-go/internal/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/platform"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func policyRunner(t *testing.T, url, class string, model agentruntime.Model) *agentruntime.Runner {
	t.Helper()
	data, err := os.ReadFile("../configs/journeys.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err = json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	server := config["mcp_servers"].([]any)[0].(map[string]any)
	server["endpoint"] = url
	server["policies"] = map[string]any{"lookup_destination": map[string]any{"class": class, "deduplication_evidence": "fixture verifies same X-Platform-Call-ID yields one mutation"}}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := os.ReadFile("../configs/prompts/planner.md")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(dir, "prompts"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "prompts/planner.md"), prompt, 0600); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(config)
	path := filepath.Join(dir, "journeys.yaml")
	if err = os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	registry, err := definitions.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return agentruntime.NewRunner(registry, platformmcp.NewClient(), model)
}

func TestToolCallLostAfterEffectNoRetry(t *testing.T) {
	pool := database(t)
	var dispatch, models atomic.Int32
	// Stateful SDK session survives an HTTP response lost after the handler applies
	// its mutation. The client receives a transport error, not an MCP business error.
	sdk := mcp.NewServer(&mcp.Implementation{Name: "effects", Version: "1"}, nil)
	mcp.AddTool(sdk, &mcp.Tool{Name: "lookup_destination", Description: "effect"}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
		dispatch.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "applied"}}}, nil, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sdk }, nil)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.Header.Get("Mcp-Session-Id") != "" {
			body := readBody(t, r)
			r.Body = body.reader()
			if body.method() == "tools/call" {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, r)
				panic(http.ErrAbortHandler)
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer backend.Close()
	model := modelFunc(func(_ context.Context, r agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		models.Add(1)
		if len(r.ToolResults) > 0 {
			return agentruntime.ModelResponse{Text: "must not reach"}, nil
		}
		return agentruntime.ModelResponse{ToolCalls: []agentruntime.ToolCall{{CallID: "effect", Name: "lookup_destination", Arguments: json.RawMessage(`{"city":"A"}`)}, {CallID: "sibling", Name: "lookup_destination", Arguments: json.RawMessage(`{"city":"B"}`)}}}, nil
	})
	p := platform.New(policyRunner(t, backend.URL, "effectful", model), journal.New(pool))
	defer p.Close()
	c := platform.Command{ThreadID: "effect-thread", RunID: "effect-run", CommunicationID: "effect-comm", Principal: "alice", Text: "change"}
	if _, err := p.Submit(t.Context(), platform.Submission{Command: c}); err != nil {
		t.Fatal(err)
	}
	s := awaitState(t, p, c.ThreadID, c.RunID, journal.RunAwaitingInput)
	var i *journal.Interaction
	for _, r := range s.Runs {
		if r.RunID == c.RunID {
			i = r.Interaction
		}
	}
	if i == nil {
		t.Fatal("missing approval")
	}
	reply := platform.Command{ThreadID: c.ThreadID, RunID: "approve", CommunicationID: "approve", Principal: "alice", InteractionID: i.ID, ReplyKind: "approval", ReplyJSON: fmt.Sprintf(`{"decision":"approve","binding":%q}`, i.Call.Binding)}
	if _, err := p.Submit(t.Context(), platform.Submission{Command: reply}); err != nil {
		t.Fatal(err)
	}
	awaitState(t, p, c.ThreadID, reply.RunID, journal.RunFailed)
	var outcome string
	var attempts int
	if err := pool.QueryRow(t.Context(), "SELECT outcome,(SELECT count(*) FROM agent_attempts WHERE call_id=o.call_id) FROM agent_operations o").Scan(&outcome, &attempts); err != nil {
		t.Fatal(err)
	}
	if outcome != "outcome_unknown" || attempts != 1 || dispatch.Load() != 1 || models.Load() != 1 {
		t.Fatal(outcome, attempts, dispatch.Load(), models.Load())
	}
	later := c
	later.RunID = "later"
	later.CommunicationID = "later"
	if _, err := p.Submit(t.Context(), platform.Submission{Command: later}); err != nil {
		t.Fatal(err)
	}
	awaitState(t, p, c.ThreadID, later.RunID, journal.RunFailed)
	if dispatch.Load() != 1 || models.Load() != 1 {
		t.Fatal("unresolved effect replayed by later run")
	}
}

type operationKey struct{}

type requestBody []byte

func readBody(t *testing.T, r *http.Request) requestBody {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func (b requestBody) reader() io.ReadCloser { return io.NopCloser(bytes.NewReader(b)) }
func (b requestBody) method() string {
	var v struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(b, &v)
	return v.Method
}

func TestToolCallVerifiedRetryStableID(t *testing.T) {
	for _, class := range []string{"read_only", "deduplicated"} {
		t.Run(class, func(t *testing.T) {
			pool := database(t)
			var mu sync.Mutex
			ids := []string{}
			effects := map[string]bool{}
			var dispatch, mutations atomic.Int32
			sdk := mcp.NewServer(&mcp.Implementation{Name: "retry", Version: "1"}, nil)
			mcp.AddTool(sdk, &mcp.Tool{Name: "lookup_destination", Description: "lookup"}, func(ctx context.Context, r *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
				dispatch.Add(1)
				id := r.Extra.Header.Get("X-Platform-Call-ID")
				mu.Lock()
				if !effects[id] {
					effects[id] = true
					mutations.Add(1)
				}
				mu.Unlock()
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "result"}}}, nil, nil
			})
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sdk }, nil)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					body := readBody(t, r)
					r.Body = body.reader()
					if body.method() == "tools/call" {
						id := r.Header.Get("X-Platform-Call-ID")
						mu.Lock()
						ids = append(ids, id)
						first := len(ids) == 1
						// Effect deduplication is enforced inside the operation handler.
						mu.Unlock()
						if first {
							rec := httptest.NewRecorder()
							handler.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), operationKey{}, id)))
							panic(http.ErrAbortHandler)
						}
					}
				}
				handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), operationKey{}, r.Header.Get("X-Platform-Call-ID"))))
			}))
			defer backend.Close()
			model := modelFunc(func(_ context.Context, r agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
				if len(r.ToolResults) > 0 {
					return agentruntime.ModelResponse{Text: "done"}, nil
				}
				return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{CallID: "provider", Name: "lookup_destination", Arguments: json.RawMessage(`{}`)}}, nil
			})
			p := platform.New(policyRunner(t, backend.URL, class, model), journal.New(pool))
			defer p.Close()
			c := platform.Command{ThreadID: "retry-thread", RunID: "retry", CommunicationID: "retry", Principal: "alice", Text: "go"}
			if _, err := p.Submit(t.Context(), platform.Submission{Command: c}); err != nil {
				t.Fatal(err)
			}
			run := c.RunID
			if class == "deduplicated" {
				s := awaitState(t, p, c.ThreadID, c.RunID, journal.RunAwaitingInput)
				i := s.Runs[0].Interaction
				if i == nil {
					t.Fatal("missing approval")
				}
				reply := platform.Command{ThreadID: c.ThreadID, RunID: "reply", CommunicationID: "reply", Principal: "alice", InteractionID: i.ID, ReplyKind: "approval", ReplyJSON: fmt.Sprintf(`{"decision":"approve","binding":%q}`, i.Call.Binding)}
				if _, err := p.Submit(t.Context(), platform.Submission{Command: reply}); err != nil {
					t.Fatal(err)
				}
				run = reply.RunID
			}
			awaitState(t, p, c.ThreadID, run, journal.RunCompleted)
			mu.Lock()
			defer mu.Unlock()
			if len(ids) != 2 || ids[0] == "" || ids[0] != ids[1] || len(effects) != 1 || !effects[ids[0]] || mutations.Load() != 1 {
				t.Fatal(ids, effects)
			}
			var n int
			if err := pool.QueryRow(t.Context(), "SELECT count(DISTINCT attempt) FROM agent_attempts").Scan(&n); err != nil || n != 2 {
				t.Fatal(n, err)
			}
		})
	}
}

func TestToolCallPredispatchPersistenceFailure(t *testing.T) {
	pool := database(t)
	mock := newMockMCP(t)
	backend := httptest.NewServer(mock)
	defer backend.Close()
	t.Setenv("TEST_JAVA_MCP_ENDPOINT", backend.URL)
	registry, err := definitions.Load("../configs/journeys.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `CREATE FUNCTION fail_intent() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'intent unavailable'; END $$;CREATE TRIGGER fail_intent BEFORE INSERT ON agent_operations FOR EACH ROW EXECUTE FUNCTION fail_intent()`); err != nil {
		t.Fatal(err)
	}
	p := platform.New(agentruntime.NewRunner(registry, platformmcp.NewClient(), modelFunc(func(context.Context, agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{CallID: "pre", Name: "lookup_destination", Arguments: json.RawMessage(`{}`)}}, nil
	})), journal.New(pool))
	defer p.Close()
	c := platform.Command{ThreadID: "pre", RunID: "pre", CommunicationID: "pre", Principal: "alice", Text: "go"}
	if _, err := p.Submit(t.Context(), platform.Submission{Command: c}); err != nil {
		t.Fatal(err)
	}
	awaitState(t, p, c.ThreadID, c.RunID, journal.RunFailed)
	if len(mock.calls()) != 0 {
		t.Fatal("dispatched without committed intent")
	}
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_operations").Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

func TestToolCallZeroOneMultipleAndDelayed(t *testing.T) {
	for _, n := range []int{0, 1, 3} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			pool := database(t)
			mock := newMockMCP(t)
			backend := httptest.NewServer(mock)
			defer backend.Close()
			var models atomic.Int32
			entered := make(chan struct{})
			release := make(chan struct{})
			model := modelFunc(func(_ context.Context, r agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
				round := models.Add(1)
				if round == 1 {
					close(entered)
					<-release
					calls := []agentruntime.ToolCall{}
					for i := 0; i < n; i++ {
						calls = append(calls, agentruntime.ToolCall{CallID: fmt.Sprint(i), Name: "lookup_destination", Arguments: json.RawMessage(`{}`)})
					}
					if n == 0 {
						return agentruntime.ModelResponse{Text: "done"}, nil
					}
					return agentruntime.ModelResponse{ToolCalls: calls}, nil
				}
				if len(r.ToolResults) != n {
					t.Errorf("partial provider resume: got %d want %d", len(r.ToolResults), n)
				}
				return agentruntime.ModelResponse{Text: "done"}, nil
			})
			p := platform.New(policyRunner(t, backend.URL, "read_only", model), journal.New(pool))
			defer p.Close()
			c := platform.Command{ThreadID: "multi", RunID: "multi", CommunicationID: "multi", Principal: "alice", Text: "go"}
			if _, err := p.Submit(t.Context(), platform.Submission{Command: c}); err != nil {
				t.Fatal(err)
			}
			<-entered
			if len(mock.calls()) != 0 {
				t.Fatal("premature dispatch")
			}
			close(release)
			awaitState(t, p, c.ThreadID, c.RunID, journal.RunCompleted)
			want := int32(2)
			if n == 0 {
				want = 1
			}
			if len(mock.calls()) != n || models.Load() != want {
				t.Fatal(len(mock.calls()), models.Load())
			}
			ids := map[string]bool{}
			for _, c := range mock.calls() {
				ids[c.callID] = true
			}
			if len(ids) != n {
				t.Fatal("call IDs reused", ids)
			}
		})
	}
}

func TestApprovalFreshCredentialsAndRejectedReplies(t *testing.T) {
	for _, decision := range []string{"approve", "deny", "timeout"} {
		t.Run(decision, func(t *testing.T) {
			pool := database(t)
			mock := newMockMCP(t)
			backend := httptest.NewServer(mock)
			defer backend.Close()
			p := platform.New(policyRunner(t, backend.URL, "effectful", modelFunc(func(_ context.Context, r agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
				if len(r.ToolResults) > 0 {
					return agentruntime.ModelResponse{Text: "done"}, nil
				}
				return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{CallID: "write", Name: "lookup_destination", Arguments: json.RawMessage(`{"amount":9007199254740993}`)}}, nil
			})), journal.New(pool))
			defer p.Close()
			c := platform.Command{ThreadID: "approval", RunID: "approval", CommunicationID: "approval", Principal: "alice", Text: "change"}
			if _, err := p.Submit(t.Context(), platform.Submission{Command: c, Headers: http.Header{"Authorization": {"old-live-auth"}}}); err != nil {
				t.Fatal(err)
			}
			s := awaitState(t, p, c.ThreadID, c.RunID, journal.RunAwaitingInput)
			i := s.Runs[0].Interaction
			if i == nil {
				t.Fatal("missing approval")
			}
			reply := platform.Command{ThreadID: c.ThreadID, RunID: "reply", CommunicationID: "reply", Principal: "alice", InteractionID: i.ID, ReplyKind: "approval"}
			for _, bad := range []string{`{"decision":"approve","binding":"forged"}`, `{"answers":{"free":{"custom":"approve"}}}`, fmt.Sprintf(`{"decision":"approve","binding":%q,"arguments":{}}`, i.Call.Binding)} {
				reply.ReplyJSON = bad
				if _, err := p.Submit(t.Context(), platform.Submission{Command: reply}); !errors.Is(err, journal.ErrState) {
					t.Fatal("invalid approval accepted", err)
				}
			}
			if decision == "timeout" {
				if _, err := pool.Exec(t.Context(), "UPDATE agent_interactions SET expires_at=clock_timestamp()-interval '1 second'"); err != nil {
					t.Fatal(err)
				}
				reply.ReplyJSON = fmt.Sprintf(`{"decision":"approve","binding":%q}`, i.Call.Binding)
				if _, err := p.Submit(t.Context(), platform.Submission{Command: reply}); !errors.Is(err, journal.ErrConflict) {
					t.Fatal(err)
				}
			} else {
				reply.ReplyJSON = fmt.Sprintf(`{"decision":%q,"binding":%q}`, decision, i.Call.Binding)
				if _, err := p.Submit(t.Context(), platform.Submission{Command: reply, Headers: http.Header{"Authorization": {"fresh-live-auth"}}}); err != nil {
					t.Fatal(err)
				}
				awaitState(t, p, c.ThreadID, reply.RunID, journal.RunCompleted)
			}
			calls := mock.calls()
			if decision == "approve" {
				if len(calls) != 1 || calls[0].authorization != "fresh-live-auth" {
					t.Fatal("approval did not use fresh owner credentials")
				}
			} else if len(calls) != 0 {
				t.Fatal("non-approval dispatched")
			}
			var persisted string
			if err := pool.QueryRow(t.Context(), "SELECT concat((SELECT jsonb_agg(to_jsonb(c))::text FROM agent_commands c),(SELECT jsonb_agg(to_jsonb(i))::text FROM agent_interactions i),(SELECT jsonb_agg(to_jsonb(s))::text FROM agent_sessions s))").Scan(&persisted); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(persisted, "live-auth") {
				t.Fatal("credentials persisted")
			}
		})
	}
}

type capturedMCP struct {
	handler  http.Handler
	mu       sync.Mutex
	observed []mcpObservation
}

func newMockMCP(t *testing.T) *capturedMCP {
	t.Helper()
	captured := new(capturedMCP)
	sdk := mcp.NewServer(&mcp.Implementation{Name: "captured", Version: "1"}, nil)
	mcp.AddTool(sdk, &mcp.Tool{Name: "lookup_destination"}, func(_ context.Context, r *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
		captured.mu.Lock()
		captured.observed = append(captured.observed, mcpObservation{callID: r.Extra.Header.Get("X-Platform-Call-ID"), authorization: r.Extra.Header.Get("Authorization")})
		captured.mu.Unlock()
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "result"}}}, nil, nil
	})
	captured.handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sdk }, nil)
	return captured
}
func (c *capturedMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) { c.handler.ServeHTTP(w, r) }
func (c *capturedMCP) calls() []mcpObservation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]mcpObservation(nil), c.observed...)
}

func TestBusinessErrorChangedActionRequiresNewApproval(t *testing.T) {
	pool := database(t)
	var calls, models atomic.Int32
	sdk := mcp.NewServer(&mcp.Implementation{Name: "reject", Version: "1"}, nil)
	mcp.AddTool(sdk, &mcp.Tool{Name: "lookup_destination"}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
		calls.Add(1)
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "private rejection details"}}}, nil, nil
	})
	backend := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sdk }, nil))
	defer backend.Close()
	p := platform.New(policyRunner(t, backend.URL, "effectful", modelFunc(func(_ context.Context, r agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		round := models.Add(1)
		return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{CallID: fmt.Sprintf("call-%d", round), Name: "lookup_destination", Arguments: json.RawMessage(fmt.Sprintf(`{"attempt":%d}`, round))}}, nil
	})), journal.New(pool))
	defer p.Close()
	c := platform.Command{ThreadID: "repair", RunID: "repair", CommunicationID: "repair", Principal: "alice", Text: "go"}
	if _, err := p.Submit(t.Context(), platform.Submission{Command: c}); err != nil {
		t.Fatal(err)
	}
	s := awaitState(t, p, c.ThreadID, c.RunID, journal.RunAwaitingInput)
	i := s.Runs[0].Interaction
	approve := func(id string, wait *journal.Interaction) platform.Command {
		return platform.Command{ThreadID: c.ThreadID, RunID: id, CommunicationID: id, Principal: "alice", InteractionID: wait.ID, ReplyKind: "approval", ReplyJSON: fmt.Sprintf(`{"decision":"approve","binding":%q}`, wait.Call.Binding)}
	}
	reply := approve("reply1", i)
	if _, err := p.Submit(t.Context(), platform.Submission{Command: reply}); err != nil {
		t.Fatal(err)
	}
	s = awaitState(t, p, c.ThreadID, reply.RunID, journal.RunAwaitingInput)
	var next *journal.Interaction
	for _, r := range s.Runs {
		if r.RunID == reply.RunID {
			next = r.Interaction
		}
	}
	if next == nil || next.Call.Binding == i.Call.Binding || next.Call.CallID == i.Call.CallID || calls.Load() != 2 || models.Load() != 2 {
		t.Fatal("repair did not request new exact approval", calls.Load(), models.Load())
	}
	bad := approve("reply2", next)
	bad.ReplyJSON = reply.ReplyJSON
	if _, err := p.Submit(t.Context(), platform.Submission{Command: bad}); !errors.Is(err, journal.ErrState) {
		t.Fatal("old approval applied to changed arguments", err)
	}
	reply = approve("reply2", next)
	if _, err := p.Submit(t.Context(), platform.Submission{Command: reply}); err != nil {
		t.Fatal(err)
	}
	awaitState(t, p, c.ThreadID, reply.RunID, journal.RunFailed)
	if calls.Load() != 4 || models.Load() != 2 {
		t.Fatal("repair budget reset across wait", calls.Load(), models.Load())
	}
	var unknown int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_operations WHERE outcome='outcome_unknown'").Scan(&unknown); err != nil || unknown != 0 {
		t.Fatal("business errors became uncertainty", unknown, err)
	}
}

func TestToolCallOwnerDatabaseLossRecordsUnknownAndInterrupted(t *testing.T) {
	pool := database(t)
	ownerPool, err := pgxpool.NewWithConfig(t.Context(), pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer ownerPool.Close()
	entered, release, applied := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls, models atomic.Int32
	sdk := mcp.NewServer(&mcp.Implementation{Name: "loss", Version: "1"}, nil)
	mcp.AddTool(sdk, &mcp.Tool{Name: "lookup_destination"}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
		close(entered)
		<-release
		calls.Add(1)
		close(applied)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "applied after owner loss"}}}, nil, nil
	})
	backend := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sdk }, nil))
	defer backend.Close()
	p := platform.New(policyRunner(t, backend.URL, "read_only", modelFunc(func(context.Context, agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		models.Add(1)
		return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{CallID: "loss", Name: "lookup_destination", Arguments: json.RawMessage(`{}`)}}, nil
	})), journal.New(ownerPool))
	defer p.Close()
	c := platform.Command{ThreadID: "loss", RunID: "loss", CommunicationID: "loss", Principal: "alice", Text: "go"}
	if _, err := p.Submit(t.Context(), platform.Submission{Command: c}); err != nil {
		t.Fatal(err)
	}
	<-entered
	ownerPool.Close()
	if _, err := pool.Exec(t.Context(), "UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id='loss'"); err != nil {
		t.Fatal(err)
	}
	if err := journal.New(pool).Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-applied
	p.Close()
	var run, outcome, attempt string
	if err := pool.QueryRow(t.Context(), "SELECT r.state,o.outcome,a.outcome FROM agent_runs r JOIN agent_operations o ON o.run_id=r.id JOIN agent_attempts a ON a.call_id=o.call_id").Scan(&run, &outcome, &attempt); err != nil || run != "interrupted" || outcome != "outcome_unknown" || attempt != "outcome_unknown" || calls.Load() != 1 || models.Load() != 1 {
		t.Fatal(run, outcome, attempt, calls.Load(), models.Load(), err)
	}
}

func TestToolCallPartialProviderFailureIsNotVisible(t *testing.T) {
	pool := database(t)
	var models atomic.Int32
	p := platform.New(fixtureRunner(t, modelFunc(func(context.Context, agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		models.Add(1)
		return agentruntime.ModelResponse{Text: "private partial output", ToolCall: &agentruntime.ToolCall{CallID: "partial", Name: "lookup_destination", Arguments: json.RawMessage(`{}`)}}, errors.New("private stream failure")
	})), journal.New(pool))
	defer p.Close()
	c := platform.Command{ThreadID: "partial", RunID: "partial", CommunicationID: "partial", Principal: "alice", Text: "go"}
	if _, err := p.Submit(t.Context(), platform.Submission{Command: c}); err != nil {
		t.Fatal(err)
	}
	s := awaitState(t, p, c.ThreadID, c.RunID, journal.RunFailed)
	if s.Answer != "" || models.Load() != 1 {
		t.Fatal("partial response replayed or exposed")
	}
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_operations").Scan(&n); err != nil || n != 0 {
		t.Fatal("partial tool response dispatched", n, err)
	}
	for _, e := range s.Events {
		if strings.Contains(e.Answer, "private") {
			t.Fatal("private partial answer emitted")
		}
	}
}
