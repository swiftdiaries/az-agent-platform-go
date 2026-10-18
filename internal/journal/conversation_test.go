package journal

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/pgtest"
)

func servedAll(string) bool { return true }

func servedOnly(digests ...string) func(string) bool {
	return func(d string) bool {
		for _, want := range digests {
			if d == want {
				return true
			}
		}
		return false
	}
}

func servedExcept(digests ...string) func(string) bool {
	only := servedOnly(digests...)
	return func(d string) bool { return !only(d) }
}

func sameJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want %q: %v", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("json = %s, want %s", got, want)
	}
}

func conversationHistory(t *testing.T, s *Store, thread string) string {
	t.Helper()
	var h string
	if err := s.pool.QueryRow(context.Background(), "SELECT history::text FROM agent_conversations WHERE id=$1", thread).Scan(&h); err != nil {
		t.Fatal(err)
	}
	return h
}

func conversationPin(t *testing.T, s *Store, thread string) (pin string, pinned bool) {
	t.Helper()
	var p *string
	if err := s.pool.QueryRow(context.Background(), "SELECT definition_digest FROM agent_conversations WHERE id=$1", thread).Scan(&p); err != nil {
		t.Fatal(err)
	}
	if p == nil {
		return "", false
	}
	return *p, true
}

func currentRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), "SELECT count(*) FROM agent_definition_current").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// drain materializes and includes the owner's pending input, as a provider adapter would.
func drain(t *testing.T, s *Store, o Owner) Inbox {
	t.Helper()
	ctx := context.Background()
	inbox, err := s.Pending(ctx, o)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if err := s.Included(ctx, o, inbox.Commands); err != nil {
		t.Fatalf("Included: %v", err)
	}
	return inbox
}

func threadCommand(thread, run, comm, text string) Command {
	return Command{ThreadID: thread, RunID: run, CommunicationID: comm, Principal: "p", Tenant: "tenant-a", Text: text}
}

func TestConversationStartPinsAndSharesHistory(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	d1, d2 := strings.Repeat("d", 64), strings.Repeat("e", 64)
	const h1 = `[{"role":"user","contents":[{"type":"text","text":"H1"}]}]`
	const h2 = `[{"role":"user","contents":[{"type":"text","text":"H2"}]}]`

	o1 := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hello"))
	if _, err := s.Start(ctx, o1, "A", d1[:63]); !errors.Is(err, ErrDefinition) {
		t.Fatalf("63-char digest: %v", err)
	}
	if _, pinned := conversationPin(t, s, "t1"); pinned || currentRows(t, s) != 0 {
		t.Fatal("rejected short digest wrote state")
	}
	history, err := s.Start(ctx, o1, "A", d1)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, string(history), `[]`)
	if pin, ok := conversationPin(t, s, "t1"); !ok || pin != d1 {
		t.Fatalf("conversation pin = %q %v", pin, ok)
	}
	if cur, err := s.CurrentDefinition(ctx); err != nil || cur != d1 {
		t.Fatalf("current = %q %v", cur, err)
	}
	var journey, digest string
	if err := s.pool.QueryRow(ctx, "SELECT journey_id,definition_digest FROM agent_runs WHERE id='r1'").Scan(&journey, &digest); err != nil || journey != "A" || digest != d1 {
		t.Fatalf("run = %q %q %v", journey, digest, err)
	}
	if _, err := s.Start(ctx, o1, "A", d2); !errors.Is(err, ErrDefinition) {
		t.Fatalf("different digest on a pinned conversation: %v", err)
	}
	// the same journey and digest again is idempotent
	if _, err := s.Start(ctx, o1, "A", d1); err != nil {
		t.Fatalf("repeat Start: %v", err)
	}
	// the run is bound to journey A
	if _, err := s.Start(ctx, o1, "B", d1); !errors.Is(err, ErrDefinition) {
		t.Fatalf("run rebound to another journey: %v", err)
	}

	// an unpinned conversation cannot pin to a digest other than current
	oOther := admitClaim(t, s, threadCommand("t-other", "r-other", "c-other", "hi"))
	if _, err := s.Start(ctx, oOther, "A", d2); !errors.Is(err, ErrDefinition) {
		t.Fatalf("unpinned conversation, current != digest: %v", err)
	}
	if _, pinned := conversationPin(t, s, "t-other"); pinned {
		t.Fatal("rejected Start pinned the conversation")
	}
	if cur, _ := s.CurrentDefinition(ctx); cur != d1 {
		t.Fatalf("current changed to %q", cur)
	}

	// finish run 1 with history H1 (include the pending command first)
	if inbox := drain(t, s, o1); len(inbox.Commands) != 1 {
		t.Fatalf("pending = %d", len(inbox.Commands))
	}
	if err := s.Finish(ctx, o1, RunCompleted, json.RawMessage(h1), "answer", "", ""); err != nil {
		t.Fatal(err)
	}
	sameJSON(t, conversationHistory(t, s, "t1"), h1)

	// a run on another journey sees the same single history
	o2 := admitClaim(t, s, threadCommand("t1", "r2", "c2", "again"))
	history, err = s.Start(ctx, o2, "B", d1)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, string(history), h1)

	// Wait writes the conversation history
	drain(t, s, o2)
	if err := s.RecordRunScope(ctx, o2, RunScope{ProductVersion: testProductVersion, EndpointID: "e1", Settings: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	in := boundClarification(t, s, o2, "i-clar", "call-clar", "")
	if err := s.Wait(ctx, o2, in, json.RawMessage(seedCheckpoint), json.RawMessage(h2)); err != nil {
		t.Fatal(err)
	}
	sameJSON(t, conversationHistory(t, s, "t1"), h2)
	var archived int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM pg_class WHERE relname='agent_sessions'").Scan(&archived); err != nil || archived != 0 {
		t.Fatalf("agent_sessions exists: %d %v", archived, err)
	}
}

func TestConversationDefinitionBySetDigest(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	b1, b2 := strings.Repeat("1", 64), strings.Repeat("2", 64)

	o1 := admitClaim(t, s, threadCommand("t1", "r1", "c1", "one"))
	o2 := admitClaim(t, s, threadCommand("t2", "r2", "c2", "two"))

	t.Run("bootstrap preconditions write nothing", func(t *testing.T) {
		for name, tc := range map[string]struct {
			bootstrap string
			served    func(string) bool
		}{
			"short":      {b1[:63], servedAll},
			"nil served": {b1, nil},
			"unserved":   {b1, servedOnly(b2)},
		} {
			if got, err := s.ResolveDefinition(ctx, o1, tc.bootstrap, tc.served); !errors.Is(err, ErrDefinition) || got != "" {
				t.Fatalf("%s: %q %v", name, got, err)
			}
		}
		if _, pinned := conversationPin(t, s, "t1"); pinned || currentRows(t, s) != 0 {
			t.Fatal("rejected resolve wrote state")
		}
		if _, err := s.CurrentDefinition(ctx); !errors.Is(err, ErrDefinition) {
			t.Fatalf("CurrentDefinition on empty: %v", err)
		}
	})

	t.Run("bootstrap, share current, keep pin", func(t *testing.T) {
		got, err := s.ResolveDefinition(ctx, o1, b1, servedAll)
		if err != nil || got != b1 {
			t.Fatalf("bootstrap: %q %v", got, err)
		}
		if cur, err := s.CurrentDefinition(ctx); err != nil || cur != b1 {
			t.Fatalf("current = %q %v", cur, err)
		}
		if pin, ok := conversationPin(t, s, "t1"); !ok || pin != b1 {
			t.Fatalf("pin = %q %v", pin, ok)
		}
		// a second conversation with a different bootstrap resolves to the existing current
		got, err = s.ResolveDefinition(ctx, o2, b2, servedAll)
		if err != nil || got != b1 {
			t.Fatalf("second conversation: %q %v", got, err)
		}
		if pin, _ := conversationPin(t, s, "t2"); pin != b1 {
			t.Fatalf("t2 pin = %q", pin)
		}
		if err := s.ActivateDefinition(ctx, b1, b2, servedAll); err != nil {
			t.Fatal(err)
		}
		// an idle pinned conversation re-pins to the advanced current (M6)
		got, err = s.ResolveDefinition(ctx, o1, b2, servedAll)
		if err != nil || got != b2 {
			t.Fatalf("idle pinned conversation: %q %v", got, err)
		}
		if pin, _ := conversationPin(t, s, "t1"); pin != b2 {
			t.Fatalf("t1 pin = %q", pin)
		}
		// a new conversation pins to the advanced current
		o3 := admitClaim(t, s, threadCommand("t3", "r3", "c3", "three"))
		got, err = s.ResolveDefinition(ctx, o3, b1, servedAll)
		if err != nil || got != b2 {
			t.Fatalf("new conversation after activation: %q %v", got, err)
		}
	})

	t.Run("unserved current or pin writes nothing", func(t *testing.T) {
		o4 := admitClaim(t, s, threadCommand("t4", "r4", "c4", "four"))
		// current is b2
		if got, err := s.ResolveDefinition(ctx, o4, b2, servedOnly(b1)); !errors.Is(err, ErrDefinition) || got != "" {
			t.Fatalf("unserved current: %q %v", got, err)
		}
		if _, pinned := conversationPin(t, s, "t4"); pinned {
			t.Fatal("unserved current pinned the conversation")
		}
		// t2 is idle and pinned to b1; current b2 is unserved
		if got, err := s.ResolveDefinition(ctx, o2, b2, servedOnly(b1)); !errors.Is(err, ErrDefinition) || got != "" {
			t.Fatalf("unserved current for idle pin: %q %v", got, err)
		}
		if pin, _ := conversationPin(t, s, "t2"); pin != b1 {
			t.Fatalf("t2 pin = %q", pin)
		}
		// a non-idle conversation keeps its unserved pin, which fails
		insertOperation(t, s, "u1", "t2", "r2", "outcome_unknown")
		if got, err := s.ResolveDefinition(ctx, o2, b2, servedOnly(b2)); !errors.Is(err, ErrDefinition) || got != "" {
			t.Fatalf("unserved pin: %q %v", got, err)
		}
		if cur, _ := s.CurrentDefinition(ctx); cur != b2 {
			t.Fatalf("current = %q", cur)
		}
	})

	t.Run("fenced by the owner lease", func(t *testing.T) {
		o5 := admitClaim(t, s, threadCommand("t5", "r5", "c5", "five"))
		mustExec(t, s.pool, "UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id='r5'")
		if _, err := s.ResolveDefinition(ctx, o5, b1, servedAll); !errors.Is(err, ErrOwnership) {
			t.Fatalf("expired owner: %v", err)
		}
		if _, pinned := conversationPin(t, s, "t5"); pinned {
			t.Fatal("expired owner pinned the conversation")
		}
	})
}

func TestConversationActivateDefinition(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	c1, c2 := strings.Repeat("3", 64), strings.Repeat("4", 64)
	current := func() string {
		cur, err := s.CurrentDefinition(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return cur
	}

	for name, tc := range map[string]struct {
		candidate string
		served    func(string) bool
	}{
		"short candidate":    {c1[:63], servedAll},
		"nil served":         {c1, nil},
		"unserved candidate": {c1, servedOnly(c2)},
	} {
		if err := s.ActivateDefinition(ctx, "", tc.candidate, tc.served); !errors.Is(err, ErrDefinition) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if currentRows(t, s) != 0 {
		t.Fatal("rejected activation inserted current")
	}
	if err := s.ActivateDefinition(ctx, c2, c1, servedAll); !errors.Is(err, ErrDefinition) {
		t.Fatalf("absent current with expected set: %v", err)
	}
	if err := s.ActivateDefinition(ctx, "", c1, servedAll); err != nil {
		t.Fatal(err)
	}
	if current() != c1 {
		t.Fatalf("current = %q", current())
	}
	if err := s.ActivateDefinition(ctx, "", c2, servedAll); !errors.Is(err, ErrDefinition) || current() != c1 {
		t.Fatalf("present current with expected empty: %v %q", err, current())
	}
	if err := s.ActivateDefinition(ctx, c2, c2, servedAll); !errors.Is(err, ErrDefinition) || current() != c1 {
		t.Fatalf("wrong expected: %v %q", err, current())
	}
	if err := s.ActivateDefinition(ctx, c1, c2, servedOnly(c2)); !errors.Is(err, ErrDefinition) || current() != c1 {
		t.Fatalf("unserved present current: %v %q", err, current())
	}
	// an idle conversation pinned to an unserved digest does not block activation
	old := strings.Repeat("5", 64)
	mustExec(t, s.pool, "INSERT INTO agent_conversations(id,principal,definition_digest) VALUES('pinned','p',$1)", old)
	mustExec(t, s.pool, "INSERT INTO agent_conversations(id,principal) VALUES('unpinned','p')")
	// a pending approval pinned to it does
	o := admitClaim(t, s, threadCommand("waiting", "rw", "cw", "hi"))
	mustExec(t, s.pool, "UPDATE agent_conversations SET definition_digest=$1 WHERE id='waiting'", old)
	rawInteraction(t, s, o, approvalArtifact("iw", "callw", "j"), "j", "pending", "", "")
	if err := s.ActivateDefinition(ctx, c1, c2, servedOnly(c1, c2)); !errors.Is(err, ErrDefinition) || current() != c1 {
		t.Fatalf("unserved pending-approval pin: %v %q", err, current())
	}
	mustExec(t, s.pool, "UPDATE agent_interactions SET state='expired' WHERE id='iw'")
	if err := s.ActivateDefinition(ctx, c1, c2, servedOnly(c1, c2)); err != nil {
		t.Fatal(err)
	}
	if current() != c2 {
		t.Fatalf("current = %q", current())
	}
}

func TestConversationDefinitionsReady(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	c1, p1 := strings.Repeat("6", 64), strings.Repeat("7", 64)
	ready := func(name string, want bool, served func(string) bool) {
		t.Helper()
		if got, err := s.DefinitionsReady(ctx, served); err != nil || got != want {
			t.Fatalf("%s: ready=%v err=%v, want %v", name, got, err, want)
		}
	}

	if got, err := s.DefinitionsReady(ctx, nil); got || !errors.Is(err, ErrDefinition) {
		t.Fatalf("nil served: %v %v", got, err)
	}
	ready("empty database", true, servedOnly())
	mustExec(t, s.pool, "INSERT INTO agent_conversations(id,principal) VALUES('null-pin','p')")
	ready("NULL pin ignored", true, servedOnly())
	mustExec(t, s.pool, "INSERT INTO agent_definition_current(definition_digest) VALUES($1)", c1)
	ready("served current", true, servedOnly(c1))
	ready("unserved current", false, servedOnly())

	// an idle conversation pinned to an unserved digest does not block readiness
	o := admitClaim(t, s, threadCommand("w", "rw", "cw", "hi"))
	mustExec(t, s.pool, "UPDATE agent_conversations SET definition_digest=$1 WHERE id='w'", p1)
	ready("idle unserved pin", true, servedOnly(c1))

	// a pending interaction pins it
	rawInteraction(t, s, o, approvalArtifact("i1", "call1", "j"), "j", "pending", "", "")
	ready("pending interaction, unserved pin", false, servedOnly(c1))
	ready("pending interaction, served pin", true, servedOnly(c1, p1))

	// a consumed interaction counts while its continuation run is live
	mustExec(t, s.pool, "UPDATE agent_interactions SET state='consumed', continuation_run_id='rw' WHERE id='i1'")
	ready("consumed, continuation pending", false, servedOnly(c1))
	mustExec(t, s.pool, "UPDATE agent_runs SET state='running' WHERE id='rw'")
	ready("consumed, continuation running", false, servedOnly(c1))
	mustExec(t, s.pool, "UPDATE agent_runs SET state='completed' WHERE id='rw'")
	ready("consumed, continuation completed", true, servedOnly(c1))

	// an unresolved operation counts
	for _, outcome := range []string{"dispatching", "outcome_unknown"} {
		insertOperation(t, s, "op-"+outcome, "w", "rw", outcome)
		ready("operation "+outcome, false, servedOnly(c1))
		mustExec(t, s.pool, "UPDATE agent_operations SET outcome='completed' WHERE call_id='op-'||$1::text", outcome)
		ready("operation completed after "+outcome, true, servedOnly(c1))
	}
}

func insertOperation(t *testing.T, s *Store, call, thread, run, outcome string) {
	t.Helper()
	mustExec(t, s.pool, "INSERT INTO agent_operations(call_id,thread_id,run_id,alias,arguments,binding,policy,outcome,requires_approval) VALUES($1,$2,$3,'x','{}','b','effectful',$4,true)", call, thread, run, outcome)
}

// repinFixture claims thread "t" pinned to d1 with current d2.
func repinFixture(t *testing.T) (*Store, Owner, string, string) {
	t.Helper()
	s := scopedStore(t)
	d1, d2 := strings.Repeat("a", 64), strings.Repeat("b", 64)
	o := admitClaim(t, s, threadCommand("t", "r1", "c1", "hi"))
	mustExec(t, s.pool, "UPDATE agent_conversations SET definition_digest=$1 WHERE id='t'", d1)
	mustExec(t, s.pool, "INSERT INTO agent_definition_current(definition_digest) VALUES($1)", d2)
	return s, o, d1, d2
}

func definitionChanged(t *testing.T, s *Store) (runs, details []string) {
	t.Helper()
	rows, err := s.pool.Query(context.Background(), "SELECT run_id, detail::text FROM agent_events WHERE kind='definition_changed' ORDER BY sequence")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var run, detail string
		if err := rows.Scan(&run, &detail); err != nil {
			t.Fatal(err)
		}
		runs, details = append(runs, run), append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return runs, details
}

func expectKept(t *testing.T, s *Store, o Owner, d1 string) {
	t.Helper()
	got, err := s.ResolveDefinition(context.Background(), o, d1, servedAll)
	if err != nil || got != d1 {
		t.Fatalf("ResolveDefinition = %q %v, want kept %q", got, err, d1)
	}
	if pin, _ := conversationPin(t, s, "t"); pin != d1 {
		t.Fatalf("pin = %q, want %q", pin, d1)
	}
	if runs, _ := definitionChanged(t, s); len(runs) != 0 {
		t.Fatalf("unexpected definition_changed events: %v", runs)
	}
}

func TestRepinIdle(t *testing.T) {
	ctx := context.Background()
	s, o, d1, d2 := repinFixture(t)
	got, err := s.ResolveDefinition(ctx, o, d1, servedAll)
	if err != nil || got != d2 {
		t.Fatalf("ResolveDefinition = %q %v, want %q", got, err, d2)
	}
	if pin, _ := conversationPin(t, s, "t"); pin != d2 {
		t.Fatalf("pin = %q", pin)
	}
	runs, details := definitionChanged(t, s)
	if len(runs) != 1 || runs[0] != "r1" {
		t.Fatalf("events = %v", runs)
	}
	sameJSON(t, details[0], `{"old_digest":"`+d1+`","new_digest":"`+d2+`"}`)
	// pin == current: no further event
	if got, err := s.ResolveDefinition(ctx, o, d1, servedAll); err != nil || got != d2 {
		t.Fatalf("second resolve = %q %v", got, err)
	}
	if runs, _ := definitionChanged(t, s); len(runs) != 1 {
		t.Fatalf("events after no-op = %v", runs)
	}
}

func TestRepinKeepsPendingApproval(t *testing.T) {
	s, o, d1, _ := repinFixture(t)
	rawInteraction(t, s, o, approvalArtifact("i1", "call1", "j"), "j", "pending", "", "")
	expectKept(t, s, o, d1)
}

func TestRepinKeepsReplyContinuation(t *testing.T) {
	s, o, d1, _ := repinFixture(t)
	rawInteraction(t, s, o, approvalArtifact("i1", "call1", "j"), "j", "consumed", "r1", `{"decision":"approve"}`)
	o.Command.InteractionID = "i1"
	expectKept(t, s, o, d1)
}

func TestRepinKeepsUnresolvedOperation(t *testing.T) {
	for _, outcome := range []string{"dispatching", "outcome_unknown"} {
		t.Run(outcome, func(t *testing.T) {
			s, o, d1, _ := repinFixture(t)
			insertOperation(t, s, "op", "t", "r1", outcome)
			expectKept(t, s, o, d1)
		})
	}
}

func TestRepinUnservedCurrent(t *testing.T) {
	ctx := context.Background()
	s, o, d1, _ := repinFixture(t)
	if got, err := s.ResolveDefinition(ctx, o, d1, servedOnly(d1)); !errors.Is(err, ErrDefinition) || got != "" {
		t.Fatalf("idle, unserved current: %q %v", got, err)
	}
	if pin, _ := conversationPin(t, s, "t"); pin != d1 {
		t.Fatalf("pin = %q", pin)
	}
	if runs, _ := definitionChanged(t, s); len(runs) != 0 {
		t.Fatalf("events = %v", runs)
	}
	// non-idle with an unserved pin
	rawInteraction(t, s, o, approvalArtifact("i1", "call1", "j"), "j", "pending", "", "")
	if got, err := s.ResolveDefinition(ctx, o, d1, servedExcept(d1)); !errors.Is(err, ErrDefinition) || got != "" {
		t.Fatalf("non-idle, unserved pin: %q %v", got, err)
	}
}

func TestRepinFencedByOwnerLease(t *testing.T) {
	s, o, d1, _ := repinFixture(t)
	mustExec(t, s.pool, "UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id='r1'")
	if _, err := s.ResolveDefinition(context.Background(), o, d1, servedAll); !errors.Is(err, ErrOwnership) {
		t.Fatalf("expired owner: %v", err)
	}
	if pin, _ := conversationPin(t, s, "t"); pin != d1 {
		t.Fatalf("pin = %q", pin)
	}
}

func TestConversationDefinitionsReadyIgnoresArchives(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Database(t)
	seedPre000009(t, ctx, pool)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	s := New(pool)
	var archived int
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM agent_sessions_archived)+(SELECT count(*) FROM agent_definition_current_archived)").Scan(&archived); err != nil || archived == 0 {
		t.Fatalf("archives: %d %v", archived, err)
	}
	ready, err := s.DefinitionsReady(ctx, servedExcept(seedDigestA, seedDigestB))
	if err != nil || !ready {
		t.Fatalf("archived digests required: %v %v", ready, err)
	}
}

func TestPendingHasNoCrossJourneyContext(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	d := strings.Repeat("d", 64)

	if n := reflect.TypeOf(Inbox{}).NumField(); n != 1 {
		t.Fatalf("Inbox has %d fields, want only Commands", n)
	}
	a := admitClaim(t, s, threadCommand("t1", "r1", "c1", "from journey A"))
	if _, err := s.Start(ctx, a, "A", d); err != nil {
		t.Fatal(err)
	}
	drain(t, s, a)
	if err := s.Finish(ctx, a, RunCompleted, json.RawMessage(`[]`), "answer from A", "", ""); err != nil {
		t.Fatal(err)
	}
	b := admitClaim(t, s, threadCommand("t1", "r2", "c2", "from journey B"))
	if _, err := s.Start(ctx, b, "B", d); err != nil {
		t.Fatal(err)
	}
	inbox, err := s.Pending(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox.Commands) != 1 || inbox.Commands[0].CommunicationID != "c2" || inbox.Commands[0].Text != "from journey B" {
		t.Fatalf("pending = %+v", inbox.Commands)
	}
}

// migratedStore builds the populated v8 database, migrates it, and returns a Store.
func migratedStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.Database(t)
	seedPre000009(t, ctx, pool)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return New(pool)
}

func TestMigrated000009PendingClarificationResumes(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	e := strings.Repeat("e", 64)
	const newHistory = `[{"role":"user","contents":[{"type":"text","text":"after clarify"}]}]`

	c := Command{ThreadID: "clarify", RunID: "clarify-reply", CommunicationID: "clarify-reply", Principal: "p",
		InteractionID: seedClarifyInteractionID, ReplyKind: "clarification", ReplyJSON: `{"answers":{"q1":{"option":"Alpha"}}}`}
	_, fresh, err := s.Admit(ctx, c)
	if err != nil || !fresh {
		t.Fatalf("Admit: fresh=%v err=%v", fresh, err)
	}
	o, err := s.Claim(ctx, c, "o1", LeaseDuration)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ResolveDefinition(ctx, o, e, servedOnly(e))
	if err != nil || got != e {
		t.Fatalf("ResolveDefinition: %q %v", got, err)
	}
	if pin, ok := conversationPin(t, s, "clarify"); !ok || pin != e {
		t.Fatalf("pin = %q %v", pin, ok)
	}
	history, err := s.Start(ctx, o, "A", e)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sameJSON(t, string(history), seedHistoryC)
	var digest string
	if err := s.pool.QueryRow(ctx, "SELECT definition_digest FROM agent_runs WHERE id='clarify-reply'").Scan(&digest); err != nil || digest != e {
		t.Fatalf("run digest = %q %v (the copied per-journey digest must be overridden)", digest, err)
	}
	cont, err := s.Continuation(ctx, o)
	if err != nil || cont == nil {
		t.Fatalf("Continuation: %+v %v", cont, err)
	}
	if cont.Interaction.ID != seedClarifyInteractionID || cont.Reply.Answers["q1"].Option != "Alpha" {
		t.Fatalf("continuation = %+v", cont)
	}
	sameJSON(t, string(cont.Checkpoint), seedCheckpoint)
	sameJSON(t, string(cont.History), seedHistoryC)
	inbox, err := s.Pending(ctx, o)
	if err != nil || len(inbox.Commands) != 1 || !inbox.Commands[0].Materialized || inbox.Commands[0].CommunicationID != "clarify-reply" {
		t.Fatalf("Pending: %+v %v", inbox, err)
	}
	if err := s.Included(ctx, o, inbox.Commands); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, o, RunCompleted, json.RawMessage(newHistory), "done", "", ""); err != nil {
		t.Fatal(err)
	}
	sameJSON(t, conversationHistory(t, s, "clarify"), newHistory)
}

// A pre-000009 approval carries a v1 binding and a per-journey definition digest. Its reply is accepted (the
// reply echoes the stored binding), but it can never match a v2 binding: it is superseded, never dispatched.
func TestMigrated000009PendingApprovalSuperseded(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	e := strings.Repeat("e", 64)
	const newHistory = `[{"role":"user","contents":[{"type":"text","text":"after approval"}]}]`

	if m := markerOf(t, s, seedApprovalInteractionID); m != "requested" {
		t.Fatalf("migrated pending approval marker = %q", m)
	}
	c := Command{ThreadID: "approval", RunID: "approval-reply", CommunicationID: "approval-reply", Principal: "p",
		InteractionID: seedApprovalInteractionID, ReplyKind: "approval", ReplyJSON: `{"decision":"approve","binding":"` + seedApprovalBinding + `"}`}
	_, fresh, err := s.Admit(ctx, c)
	if err != nil || !fresh {
		t.Fatalf("Admit: fresh=%v err=%v", fresh, err)
	}
	if m := markerOf(t, s, seedApprovalInteractionID); m != "decided" {
		t.Fatalf("marker after approve = %q", m)
	}
	o, err := s.Claim(ctx, c, "o1", LeaseDuration)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.ResolveDefinition(ctx, o, e, servedOnly(e)); err != nil || got != e {
		t.Fatalf("ResolveDefinition: %q %v", got, err)
	}
	history, err := s.Start(ctx, o, "A", e)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sameJSON(t, string(history), seedHistoryAp)
	if err := s.RecordRunScope(ctx, o, RunScope{ProductVersion: testProductVersion, EndpointID: "e1", Settings: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	cont, err := s.Continuation(ctx, o)
	if err != nil || cont == nil || cont.Interaction.ID != seedApprovalInteractionID || cont.Reply.Decision != "approve" || cont.Marker != "decided" {
		t.Fatalf("Continuation: %+v %v", cont, err)
	}
	if cont.Interaction.JourneyID != "A" || cont.Interaction.OperationKey != "" {
		t.Fatalf("legacy columns = %+v", cont.Interaction)
	}
	sameJSON(t, string(cont.History), seedHistoryAp)

	op := approvedOp(t, s, o, seedApprovalCallID, "A")
	// a different call is never silently approved by this interaction
	other := op
	other.CallID = "call-other"
	if err := s.BeginAttempt(ctx, o, other, 1, ""); !errors.Is(err, ErrState) {
		t.Fatalf("unapproved call: %v", err)
	}
	if err := s.BeginAttempt(ctx, o, other, 1, seedApprovalInteractionID); !errors.Is(err, ErrState) {
		t.Fatalf("different call under the approval id: %v", err)
	}
	if err := s.BeginAttempt(ctx, o, op, 1, seedApprovalInteractionID); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("legacy approval BeginAttempt = %v, want ErrSuperseded", err)
	}
	if m := markerOf(t, s, seedApprovalInteractionID); m != "superseded" || operationRows(t, s, seedApprovalCallID) != 0 {
		t.Fatalf("marker = %q, operation rows = %d", m, operationRows(t, s, seedApprovalCallID))
	}
	drain(t, s, o)
	if err := s.Finish(ctx, o, RunCompleted, json.RawMessage(newHistory), "done", "", ""); err != nil {
		t.Fatal(err)
	}
	sameJSON(t, conversationHistory(t, s, "approval"), newHistory)
}
