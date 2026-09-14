package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/swiftdiaries/az-agent-platform-go/internal/chat"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/platform"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func questionCall(id string) *agentruntime.ToolCall {
	return &agentruntime.ToolCall{CallID: id, Name: "request_user_input", Arguments: json.RawMessage(`{"questions":[{"id":"choice","header":"Choose","question":"Choose one","options":[{"label":"A","description":"First"},{"label":"B","description":"Second"}]}]}`)}
}
func awaitState(t *testing.T, p *platform.Platform, thread, run string, state journal.RunState) platform.Snapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s, err := p.Snapshot(context.Background(), thread, "alice")
		if err == nil {
			for _, r := range s.Runs {
				if r.RunID == run && r.State == state {
					return s
				}
				if r.RunID == run && r.State == journal.RunFailed && state != journal.RunFailed {
					t.Fatalf("run failed: %#v", s)
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("state timeout", state)
	return platform.Snapshot{}
}
func TestHITLQuestionShape(t *testing.T) {
	call := questionCall("provider")
	binding, err := journal.ActionBinding(call.Name, call.Arguments)
	if err != nil {
		t.Fatal(err)
	}
	var args struct {
		Questions []journal.Question `json:"questions"`
	}
	if err = json.Unmarshal(call.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	i := journal.Interaction{ID: "wait", Kind: "clarification", Questions: args.Questions, Call: journal.ProposedCall{CallID: "platform", ProviderCallID: call.CallID, Name: call.Name, Arguments: call.Arguments, Binding: binding}}
	if err = i.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []journal.Reply{{}, {Decision: "approve"}, {Answers: map[string]journal.Answer{"choice": {Option: "C"}}}, {Answers: map[string]journal.Answer{"choice": {Option: "A", Custom: "both"}}}} {
		if i.ValidateReply(bad) == nil {
			t.Fatal("invalid reply accepted", bad)
		}
	}
	if i.ValidateReply(journal.Reply{Answers: map[string]journal.Answer{"choice": {Custom: "free text"}}}) != nil {
		t.Fatal("custom clarification rejected")
	}
}

func TestHITLDurableWaitReply(t *testing.T) {
	pool := database(t)
	store := journal.New(pool)
	runner := fixtureRunner(t, modelFunc(func(_ context.Context, r agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		if len(r.ToolResults) == 0 {
			return agentruntime.ModelResponse{ToolCall: questionCall("question")}, nil
		}
		return agentruntime.ModelResponse{Text: "resumed"}, nil
	}))
	p := platform.New(runner, store)
	defer p.Close()
	c := platform.Command{ThreadID: "thread", RunID: "run", CommunicationID: "comm", Principal: "alice", Text: "ask"}
	if _, err := p.Submit(t.Context(), platform.Submission{Command: c}); err != nil {
		t.Fatal(err)
	}
	s := awaitState(t, p, c.ThreadID, c.RunID, journal.RunAwaitingInput)
	var wait *journal.Interaction
	for _, r := range s.Runs {
		if r.RunID == c.RunID {
			wait = r.Interaction
		}
	}
	if wait == nil {
		t.Fatal("missing interaction")
	}
	var owner string
	var live bool
	if err := pool.QueryRow(t.Context(), "SELECT owner_id,lease_until>clock_timestamp() FROM agent_runs WHERE id=$1", c.RunID).Scan(&owner, &live); err != nil || owner != "" || live {
		t.Fatal("wait retained owner", owner, live, err)
	}
	steer := c
	steer.RunID = "steer"
	steer.CommunicationID = "steer"
	if _, err := p.Submit(t.Context(), platform.Submission{Command: steer}); !errors.Is(err, journal.ErrBusy) {
		t.Fatal("waiting steering must conflict", err)
	}
	p.Close()
	p2 := platform.New(runner, journal.New(pool))
	defer p2.Close()
	reply := platform.Command{ThreadID: c.ThreadID, RunID: "reply", CommunicationID: "reply", Principal: "alice", InteractionID: wait.ID, ReplyKind: "clarification", ReplyJSON: `{"answers":{"choice":{"option":"A"}}}`}
	bad := reply
	bad.Principal = "mallory"
	if _, err := p2.Submit(t.Context(), platform.Submission{Command: bad}); !errors.Is(err, journal.ErrForbidden) {
		t.Fatal("foreign reply", err)
	}
	bad = reply
	bad.ReplyKind = "approval"
	if _, err := p2.Submit(t.Context(), platform.Submission{Command: bad}); !errors.Is(err, journal.ErrState) {
		t.Fatal("wrong kind", err)
	}
	if _, err := p2.Submit(t.Context(), platform.Submission{Command: reply}); err != nil {
		t.Fatal(err)
	}
	completed := awaitState(t, p2, c.ThreadID, reply.RunID, journal.RunCompleted)
	if completed.Answer != "resumed" {
		t.Fatal(completed)
	}
	if _, err := p2.Submit(t.Context(), platform.Submission{Command: reply}); err != nil {
		t.Fatal("duplicate receipt", err)
	}
	bad = reply
	bad.RunID = "another"
	bad.CommunicationID = "another"
	if _, err := p2.Submit(t.Context(), platform.Submission{Command: bad}); !errors.Is(err, journal.ErrConflict) {
		t.Fatal("interaction consumed twice", err)
	}
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_runs WHERE id='reply'").Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func localWait(t *testing.T, pool *pgxpool.Pool) (*journal.Store, journal.Owner, journal.Interaction) {
	t.Helper()
	s := journal.New(pool)
	c := journal.Command{ThreadID: "thread", RunID: "run", CommunicationID: "comm", Principal: "alice", Text: "ask"}
	if _, _, err := s.Admit(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	o := claimForTest(t, s, c, true)
	if _, err := s.Start(t.Context(), o, "journey", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	call := questionCall("call")
	binding, _ := journal.ActionBinding(call.Name, call.Arguments)
	var args struct {
		Questions []journal.Question `json:"questions"`
	}
	_ = json.Unmarshal(call.Arguments, &args)
	i := journal.Interaction{ID: "interaction", Kind: "clarification", Questions: args.Questions, Call: journal.ProposedCall{CallID: "operation", ProviderCallID: "call", Name: call.Name, Arguments: call.Arguments, Binding: binding}}
	return s, o, i
}
func TestReplyWaitTransactionsAndTTL(t *testing.T) {
	pool := database(t)
	store, owner, i := localWait(t, pool)
	if _, err := pool.Exec(t.Context(), `CREATE FUNCTION fail_wait() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected wait rollback'; END $$; CREATE TRIGGER fail_wait BEFORE INSERT ON agent_interactions FOR EACH ROW EXECUTE FUNCTION fail_wait()`); err != nil {
		t.Fatal(err)
	}
	if store.Wait(t.Context(), owner, i, []byte(`{}`), []byte(`[]`)) == nil {
		t.Fatal("wait should rollback")
	}
	var n int
	var state string
	if err := pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM agent_interactions),state FROM agent_runs WHERE id='run'").Scan(&n, &state); err != nil || n != 0 || state != "running" {
		t.Fatal(n, state, err)
	}
	if _, err := pool.Exec(t.Context(), "DROP TRIGGER fail_wait ON agent_interactions"); err != nil {
		t.Fatal(err)
	}
	if err := store.Wait(t.Context(), owner, i, []byte(`{}`), []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	// Execution lease is deliberately old during a wait; only interaction TTL expires it.
	if err := store.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT state FROM agent_runs WHERE id='run'").Scan(&state); err != nil || state != "awaiting_input" {
		t.Fatal(state, err)
	}
	reply := journal.Command{ThreadID: "thread", RunID: "reply", CommunicationID: "reply", Principal: "alice", InteractionID: i.ID, ReplyKind: "clarification", ReplyJSON: `{"answers":{"choice":{"option":"A"}}}`}
	if _, err := pool.Exec(t.Context(), `CREATE FUNCTION fail_reply() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id='reply' THEN RAISE EXCEPTION 'injected reply rollback'; END IF; RETURN NEW; END $$;CREATE TRIGGER fail_reply BEFORE INSERT ON agent_runs FOR EACH ROW EXECUTE FUNCTION fail_reply()`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Admit(t.Context(), reply); err == nil {
		t.Fatal("reply should rollback")
	}
	if err := pool.QueryRow(t.Context(), "SELECT state,(SELECT count(*) FROM agent_commands WHERE run_id='reply') FROM agent_interactions").Scan(&state, &n); err != nil || state != "pending" || n != 0 {
		t.Fatal(state, n, err)
	}
	if _, err := pool.Exec(t.Context(), "DROP TRIGGER fail_reply ON agent_runs;UPDATE agent_interactions SET expires_at=clock_timestamp()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Admit(t.Context(), reply); !errors.Is(err, journal.ErrConflict) {
		t.Fatal("expired reply", err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT state FROM agent_interactions").Scan(&state); err != nil || state != "expired" {
		t.Fatal(state, err)
	}
	var history string
	if err := pool.QueryRow(t.Context(), "SELECT history::text FROM agent_sessions").Scan(&history); err != nil || !strings.Contains(history, "interaction_expired") {
		t.Fatal("expired call not closed", err)
	}
}

func TestHITLSeparateProcessAfterWaitAndReplyCommit(t *testing.T) {
	if dsn := os.Getenv("TASK4_PROCESS_DATABASE"); dsn != "" {
		pool, err := pgxpool.New(t.Context(), dsn)
		if err != nil {
			t.Fatal("connect child database")
		}
		defer pool.Close()
		store := journal.New(pool)
		if os.Getenv("TASK4_PROCESS_PHASE") == "reply" {
			var c journal.Command
			if err := json.Unmarshal([]byte(os.Getenv("TASK4_PROCESS_COMMAND")), &c); err != nil {
				t.Fatal(err)
			}
			if _, fresh, err := store.Admit(t.Context(), c); err != nil || !fresh {
				t.Fatal("child admit", err)
			}
			return
		}
		p := platform.New(fixtureRunner(t, modelFunc(func(context.Context, agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
			return agentruntime.ModelResponse{ToolCall: questionCall("process-call")}, nil
		})), store)
		defer p.Close()
		if _, err := p.Submit(t.Context(), platform.Submission{Command: platform.Command{ThreadID: "process", RunID: "wait", CommunicationID: "wait", Principal: "alice", Text: "ask"}}); err != nil {
			t.Fatal(err)
		}
		awaitState(t, p, "process", "wait", journal.RunAwaitingInput)
		return
	}
	pool := database(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(pool.Config().ConnString())
	if err != nil {
		t.Fatal("child database configuration")
	}
	u.Path = "/" + pool.Config().ConnConfig.Database
	childDSN := u.String()
	child := func(phase string, c journal.Command) {
		t.Helper()
		payload, _ := json.Marshal(c)
		cmd := exec.Command(executable, "-test.run=^TestHITLSeparateProcessAfterWaitAndReplyCommit$", "-test.count=1")
		cmd.Env = append(os.Environ(), "TASK4_PROCESS_DATABASE="+childDSN, "TASK4_PROCESS_PHASE="+phase, "TASK4_PROCESS_COMMAND="+string(payload))
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child phase %s: %v %s", phase, err, output)
		}
	}
	child("wait", journal.Command{})
	var artifact []byte
	if err := pool.QueryRow(t.Context(), "SELECT artifact FROM agent_interactions WHERE state='pending'").Scan(&artifact); err != nil {
		t.Fatal(err)
	}
	var i journal.Interaction
	if err := json.Unmarshal(artifact, &i); err != nil {
		t.Fatal(err)
	}
	reply := journal.Command{ThreadID: "process", RunID: "reply", CommunicationID: "reply", Principal: "alice", InteractionID: i.ID, ReplyKind: "clarification", ReplyJSON: `{"answers":{"choice":{"custom":"fresh answer"}}}`}
	child("reply", reply)
	var models atomic.Int32
	p := platform.New(fixtureRunner(t, modelFunc(func(_ context.Context, r agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		models.Add(1)
		if len(r.ToolResults) != 1 || r.ToolResults[0].CallID != "process-call" {
			t.Errorf("lost correlated result: %+v", r.ToolResults)
		}
		return agentruntime.ModelResponse{Text: "process resumed"}, nil
	})), journal.New(pool))
	defer p.Close()
	c := platform.Command{ThreadID: reply.ThreadID, RunID: reply.RunID, CommunicationID: reply.CommunicationID, Principal: reply.Principal, InteractionID: reply.InteractionID, ReplyKind: reply.ReplyKind, ReplyJSON: reply.ReplyJSON}
	if _, err := p.Submit(t.Context(), platform.Submission{Command: c}); err != nil {
		t.Fatal(err)
	}
	awaitState(t, p, "process", "reply", journal.RunCompleted)
	if models.Load() != 1 {
		t.Fatal(models.Load())
	}
}

func TestReplyChatProjectionAndOutstandingOrdering(t *testing.T) {
	pool := database(t)
	var models atomic.Int32
	runner := fixtureRunner(t, modelFunc(func(_ context.Context, r agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		if models.Add(1) == 1 {
			return agentruntime.ModelResponse{ToolCalls: []agentruntime.ToolCall{*questionCall("first"), {CallID: "sibling", Name: "lookup_destination", Arguments: json.RawMessage(`{}`)}}}, nil
		}
		if len(r.ToolResults) != 2 {
			t.Errorf("provider resumed with partial outstanding results: %+v", r.ToolResults)
		}
		var history []*message.Message
		if err := json.Unmarshal(r.History, &history); err != nil {
			t.Error(err)
		}
		pending := map[string]bool{}
		for _, m := range history {
			for _, c := range m.Contents {
				switch content := c.(type) {
				case *message.FunctionCallContent:
					pending[content.CallID] = true
				case *message.FunctionResultContent:
					if m.Role != message.RoleTool || !pending[content.CallID] {
						t.Error("uncorrelated tool result")
					}
					delete(pending, content.CallID)
				}
			}
		}
		if len(pending) != 0 {
			t.Error("outstanding calls at provider reentry", pending)
		}
		return agentruntime.ModelResponse{Text: "answered"}, nil
	}))
	p := platform.New(runner, journal.New(pool))
	defer p.Close()
	api := httptest.NewServer(chat.NewHandler(chat.StaticBearerTokens{"token": "alice"}, p, pool))
	defer api.Close()
	first := postJSONAGUI(t, api.URL, `{"threadId":"external-thread","runId":"external-wait","messages":[{"id":"user","role":"user","content":"ask"}],"tools":[],"context":[],"state":{},"forwardedProps":{}}`, "token", "", "")
	if first.StatusCode != 200 || !strings.Contains(first.Body, "interaction.requested") {
		t.Fatal("wait projection", first.StatusCode, string(first.Body))
	}
	var artifact []byte
	if err := pool.QueryRow(t.Context(), "SELECT artifact FROM agent_interactions WHERE state='pending'").Scan(&artifact); err != nil {
		t.Fatal(err)
	}
	var i journal.Interaction
	if err := json.Unmarshal(artifact, &i); err != nil {
		t.Fatal(err)
	}
	reply := fmt.Sprintf(`{"threadId":"external-thread","runId":"external-reply","messages":[],"tools":[],"context":[],"state":{},"forwardedProps":{"interactionReply":{"interactionId":%q,"kind":"clarification","answer":{"answers":{"choice":{"option":"A"}}}}}}`, i.ID)
	answer := postJSONAGUI(t, api.URL, reply, "token", "", "")
	if answer.StatusCode != 200 || !strings.Contains(answer.Body, "answered") || !strings.Contains(answer.Body, `"runId":"external-reply"`) {
		t.Fatal("reply projection", answer.StatusCode, string(answer.Body))
	}
	if models.Load() != 2 {
		t.Fatal(models.Load())
	}
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_operations").Scan(&n); err != nil || n != 0 {
		t.Fatal("deferred sibling dispatched", n, err)
	}
}

func postJSONAGUI(t *testing.T, endpoint, raw, token, cookie, smuggled string) httpResult {
	t.Helper()
	var input aguitypes.RunAgentInput
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		t.Fatal(err)
	}
	return postAGUI(t, endpoint, input, token, cookie, smuggled)
}

func TestHITLCompletedToolNotReplayed(t *testing.T) {
	pool := database(t)
	mock := newMockMCP(t)
	backend := httptest.NewServer(mock)
	defer backend.Close()
	model := modelFunc(func(_ context.Context, r agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		switch len(r.ToolResults) {
		case 0:
			return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{CallID: "completed", Name: "lookup_destination", Arguments: json.RawMessage(`{}`)}}, nil
		case 1:
			return agentruntime.ModelResponse{ToolCall: questionCall("question")}, nil
		default:
			return agentruntime.ModelResponse{Text: "resumed without replay"}, nil
		}
	})
	p := platform.New(policyRunner(t, backend.URL, "read_only", model), journal.New(pool))
	c := platform.Command{ThreadID: "replay", RunID: "replay", CommunicationID: "replay", Principal: "alice", Text: "go"}
	if _, err := p.Submit(t.Context(), platform.Submission{Command: c}); err != nil {
		t.Fatal(err)
	}
	s := awaitState(t, p, c.ThreadID, c.RunID, journal.RunAwaitingInput)
	i := s.Runs[0].Interaction
	p.Close()
	p2 := platform.New(policyRunner(t, backend.URL, "read_only", model), journal.New(pool))
	defer p2.Close()
	reply := platform.Command{ThreadID: c.ThreadID, RunID: "reply", CommunicationID: "reply", Principal: "alice", InteractionID: i.ID, ReplyKind: "clarification", ReplyJSON: `{"answers":{"choice":{"option":"A"}}}`}
	if _, err := p2.Submit(t.Context(), platform.Submission{Command: reply}); err != nil {
		t.Fatal(err)
	}
	awaitState(t, p2, c.ThreadID, reply.RunID, journal.RunCompleted)
	if len(mock.calls()) != 1 {
		t.Fatal("completed tool replayed", len(mock.calls()))
	}
}

func TestReplyLosslessActionBinding(t *testing.T) {
	one, err := journal.ActionBinding("write", []byte(`{"n":9007199254740992,"s":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	same, err := journal.ActionBinding("write", []byte(`{"s":"a", "n":9007199254740992}`))
	if err != nil || one != same {
		t.Fatal("non-deterministic canonical binding")
	}
	different, err := journal.ActionBinding("write", []byte(`{"n":9007199254740993,"s":"a"}`))
	if err != nil || one == different {
		t.Fatal("large integer rounded in binding")
	}
}
